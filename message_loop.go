package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/pipelines"
	"sokratos/routines"
	"sokratos/tools"
)

// messageContext bundles all session-level dependencies needed by the message
// handling functions. All fields are reference types (pointers, maps, funcs)
// so passing by value is safe.
type messageContext struct {
	cfg            *config.AppConfig
	svc            *serviceBundle
	eng            *engine.Engine
	lb             *llmBundle
	registry       *tools.Registry
	emailTriageCfg *pipelines.TriageConfig
	fallbacks      llm.FallbackMap
	confirmExec    func(context.Context, json.RawMessage) (string, error)
	skillMtimes    map[string]time.Time
	rebuildGrammar func()
}

// handleReload forces a full re-sync of routines.toml and skills from disk.
// Returns a human-readable summary of what changed.
func handleReload(mc messageContext) string {
	added, updated, deleted := routines.SyncFromFile(db.Pool, "routines.toml")
	skillsChanged := tools.SyncSkills(mc.registry, "skills", mc.rebuildGrammar, mc.skillMtimes, db.Pool)
	var parts []string
	if len(added)+len(updated)+len(deleted) > 0 {
		parts = append(parts, fmt.Sprintf("Routines: +%d ~%d -%d", len(added), len(updated), len(deleted)))
	}
	if skillsChanged {
		parts = append(parts, "Skills: reloaded")
	}
	if len(parts) > 0 {
		return "Reloaded: " + strings.Join(parts, ", ")
	}
	return "Everything up to date."
}

// handleBootstrap launches a profile generation run in the background.
// Returns an immediate acknowledgement string.
func handleBootstrap(mc messageContext) string {
	if db.Pool == nil || mc.svc.DTC == nil || mc.cfg.EmbedURL == "" {
		return "Bootstrap requires database, deep thinker, and embedding service."
	}
	bootstrapSend := func(text string) {
		for id := range mc.cfg.AllowedIDs {
			m := tgbotapi.NewMessage(id, text)
			mc.svc.Bot.Send(m)
		}
	}
	go pipelines.RunBootstrap(pipelines.BootstrapConfig{
		Pool:          db.Pool,
		DTC:           mc.svc.DTC,
		EmbedEndpoint: mc.cfg.EmbedURL,
		EmbedModel:    mc.cfg.EmbedModel,
		AgentName:     mc.cfg.AgentName,
		SendFunc:      bootstrapSend,
		OnProfile: func() {
			mc.eng.RefreshProfile()
			mc.eng.RefreshPersonality()
		},
		BgGrammarFn: mc.svc.BgGrammarFunc,
	})
	return "Profile generation started in the background. I'll notify you when it's ready."
}

// processMessage runs the full orchestrator pipeline for a regular user message:
// prefetch → temporal context → orchestrator → slide/archive → triage → reply.
func processMessage(mc messageContext, msg *tgbotapi.Message, chatID int64, msgText, userPrompt string, visionParts []llm.ContentPart) {
	typingCtx, typingCancel := context.WithCancel(context.Background())
	go sendTypingPeriodically(mc.svc.Bot, chatID, typingCtx)

	// Phase 1: Snapshot history (StateManager has its own RWMutex).
	history := mc.svc.StateMgr.ReadMessages()

	// Phase 2: Prefetch (network I/O — no engine state needed).
	var prefetchContent string
	var prefetchIDs []int64
	var prefetchSummaries string
	if db.Pool != nil && mc.cfg.EmbedURL != "" && strings.TrimSpace(msgText) != "" {
		pfCtx, pfCancel := context.WithTimeout(context.Background(), tools.TimeoutPrefetch)
		if pf := subconsciousPrefetch(pfCtx, db.Pool, mc.cfg.EmbedURL, mc.cfg.EmbedModel, msgText, history); pf != nil {
			prefetchContent = pf.Summaries
			prefetchIDs = pf.IDs
			prefetchSummaries = pf.Summaries
		}
		pfCancel()
	}

	// Phase 2.5: Build temporal context (DB query — outside the lock).
	var temporalCtx string
	if db.Pool != nil {
		temporalCtx = engine.BuildTemporalContext(context.Background(), db.Pool)
	}

	// Phase 3: Snapshot personality/profile under the lock (microseconds),
	// then release before the multi-second inference call so the heartbeat
	// loop and any concurrent Telegram messages are not blocked.
	mc.eng.Mu.Lock()
	personalityContent := mc.eng.PersonalityContent
	profileContent := mc.eng.ProfileContent
	mc.eng.Mu.Unlock()

	reply, msgs, err := llm.QueryOrchestrator(context.Background(), mc.lb.Client, mc.cfg.LLMModel, userPrompt, mc.confirmExec, mc.lb.TrimFn, &llm.QueryOrchestratorOpts{
		Parts:              visionParts,
		History:            history,
		PersonalityContent: personalityContent,
		ProfileContent:     profileContent,
		TemporalContext:    temporalCtx,
		PrefetchContent:    prefetchContent,
		MaxToolResultLen:   mc.cfg.MaxToolResultLen,
		MaxWebSources:      mc.cfg.MaxWebSources,
		ToolAgent:          mc.lb.ToolAgent,
		Fallbacks:          mc.fallbacks,
	})
	typingCancel()

	condensed := condenseToolResults(msgs)
	for _, m := range condensed {
		mc.svc.StateMgr.AppendMessage(m)
	}
	if db.Pool != nil && mc.cfg.EmbedURL != "" {
		engine.SlideAndArchiveContext(context.Background(), mc.svc.StateMgr, mc.eng.MaxMessages, engine.ArchiveDeps{
			DB: db.Pool, EmbedEndpoint: mc.cfg.EmbedURL, EmbedModel: mc.cfg.EmbedModel,
			DTCQueueFn: mc.svc.DTCQueueFunc, SubagentFn: mc.svc.SubagentFunc,
			GrammarFn: mc.svc.GrammarFunc, BgGrammarFn: mc.svc.BgGrammarFunc, QueueFn: mc.svc.QueueFunc,
		})
	}

	if err == nil && db.Pool != nil && mc.cfg.EmbedURL != "" && mc.svc.DTC != nil && mc.emailTriageCfg != nil {
		toolCtx, toolsUsed := summarizeToolContext(msgs)
		exchange := toolCtx + fmt.Sprintf("user: %s\nassistant: %s", msgText, reply)
		pipelines.TriageAndSaveConversationAsync(*mc.emailTriageCfg, exchange, toolsUsed)
	}

	if err == nil && len(prefetchIDs) > 0 && db.Pool != nil && mc.svc.Subagent != nil {
		capturedIDs := prefetchIDs
		capturedReply := reply
		capturedMsgText := msgText
		capturedSubagent := mc.svc.Subagent
		capturedSummaries := prefetchSummaries
		go evaluateMemoryUsefulnessViaSubagent(db.Pool, capturedSubagent, capturedIDs, capturedMsgText, capturedReply, capturedSummaries)
	}

	if err != nil {
		logger.Log.Errorf("LLM error: %v", err)
		reply = "Sorry, something went wrong processing your message."
	}

	// Don't send orchestrator control tags to the user.
	if strings.Contains(reply, "<NO_ACTION_REQUIRED>") {
		return
	}

	// Format reply with entity-based markdown. `reply` already includes
	// accumulated intermediate text from tool-call rounds (prepended by
	// the supervisor). Thinking is logged in the supervisor, not shown in UI.
	fm := formatReply(reply)
	if _, err := sendFormatted(mc.svc.Bot, chatID, msg.MessageID, fm); err != nil {
		logger.Log.Warnf("Entity send failed, falling back to plain text: %v", err)
		replyMsg := tgbotapi.NewMessage(chatID, reply)
		replyMsg.ReplyToMessageID = msg.MessageID
		if _, err := mc.svc.Bot.Send(replyMsg); err != nil {
			logger.Log.Errorf("Error sending message: %v", err)
		}
	}
}
