package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/google"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/pipelines"
	"sokratos/routines"
	"sokratos/textutil"
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
	confirmExec    func(context.Context, json.RawMessage) (string, error)
	skillMtimes    map[string]time.Time
	skillDeps      tools.SkillDeps
	rebuildGrammar func()
	router         engine.SlotRouter
	delegateConfig *tools.DelegateConfig
	messageChan    <-chan *tgbotapi.Message // for /google auth code reads
}

// handleReload forces a full re-sync of routines.toml and skills from disk.
// Returns a human-readable summary of what changed.
func handleReload(mc messageContext) string {
	added, updated, deleted := routines.SyncFromFile(db.Pool, ".config/routines.toml")
	skillsChanged := tools.SyncSkills(mc.registry, "skills", mc.rebuildGrammar, mc.skillMtimes, mc.skillDeps)
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
		PipelineDeps: pipelines.PipelineDeps{
			Pool:          db.Pool,
			DTC:           mc.svc.DTC,
			EmbedEndpoint: mc.cfg.EmbedURL,
			EmbedModel:    mc.cfg.EmbedModel,
			GrammarFn:     mc.svc.BgGrammarFunc,
		},
		AgentName: mc.cfg.AgentName,
		SendFunc:  bootstrapSend,
		OnProfile: func() {
			mc.eng.RefreshProfile()
			mc.eng.RefreshPersonality()
		},
		QueueFn: mc.svc.QueueFunc,
	})
	return "Profile generation started in the background. I'll notify you when it's ready."
}

// handleGoogle triggers Google OAuth re-authentication via Telegram.
// Uses a single OAuth flow with combined Gmail+Calendar scopes so only
// one auth URL + code paste is needed. Re-initializes both services and
// registers tools that were previously disabled.
func handleGoogle(mc messageContext) string {
	gmailWasNil := google.GmailService == nil
	calWasNil := google.CalendarService == nil

	// Delete existing token to force a fresh OAuth flow.
	os.Remove(mc.cfg.GoogleTokenPath)

	// Build auth IO that reads from the split message channel (not the raw
	// updates channel which is drained by the splitter goroutine).
	authIO := &google.AuthIO{
		Send: func(msg string) {
			for id := range mc.cfg.AllowedIDs {
				m := tgbotapi.NewMessage(id, msg)
				mc.svc.Bot.Send(m)
			}
		},
		Receive: func() (string, error) {
			for msg := range mc.messageChan {
				if msg.Text != "" {
					return strings.TrimSpace(msg.Text), nil
				}
			}
			return "", fmt.Errorf("message channel closed")
		},
	}

	// Combined scopes for a single OAuth flow.
	scopes := []string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.send",
		"https://www.googleapis.com/auth/calendar",
	}

	client, err := google.GetClient(
		context.Background(), "Google",
		mc.cfg.GmailCredsPath, mc.cfg.GoogleTokenPath,
		scopes, authIO,
	)
	if err != nil {
		return fmt.Sprintf("❌ Google auth failed: %v", err)
	}
	if client == nil {
		return "⚠️ Google credentials file not found — features disabled"
	}

	// Create both services from the single authenticated client.
	var results []string

	if err := google.InitGmailFromClient(context.Background(), client); err != nil {
		results = append(results, fmt.Sprintf("Gmail: ❌ %v", err))
	} else {
		results = append(results, "Gmail: ✅")
	}

	if err := google.InitCalendarFromClient(context.Background(), client); err != nil {
		results = append(results, fmt.Sprintf("Calendar: ❌ %v", err))
	} else {
		results = append(results, "Calendar: ✅")
	}

	// Register tools that were previously disabled.
	if gmailWasNil && google.GmailService != nil {
		registerGmailTools(mc.registry, db.Pool, mc.emailTriageCfg, mc.cfg.EmailDisplayBatch)
		mc.rebuildGrammar()
		logger.Log.Info("[/google] Gmail tools registered")
	}
	if calWasNil && google.CalendarService != nil {
		registerCalendarTools(mc.registry, db.Pool)
		mc.rebuildGrammar()
		logger.Log.Info("[/google] Calendar tools registered")
	}

	// Reset the auth error once so future expiry triggers a new notification.
	mc.svc.AuthErrorOnce = &sync.Once{}

	// Also invalidate the calendar list cache since we have a fresh token.
	google.InvalidateCache()

	return strings.Join(results, "\n")
}

// processMessage runs the full orchestrator pipeline for a regular user message:
// prefetch → temporal context → orchestrator → slide/archive → triage → reply.
func processMessage(mc messageContext, msg *tgbotapi.Message, chatID int64, msgText, userPrompt string, visionParts []llm.ContentPart) {
	typingCtx, typingCancel := context.WithCancel(context.Background())
	go sendTypingPeriodically(mc.svc.Bot, chatID, typingCtx)

	// Phase 0: Pipeline isolation — tag this message and exclude prior pipeline's memories.
	pipelineID := int64(msg.MessageID)
	excludePipelineID := mc.svc.StateMgr.LastPipelineID()

	// Phase 1: Snapshot history (StateManager has its own RWMutex).
	history := mc.svc.StateMgr.ReadMessages()

	// Phase 1.5: Staleness trim — if there's a long gap since the last
	// conversation message, old topics are stale and will confuse the Brain.
	// Keep only recent messages for immediate context.
	const staleGap = 30 * time.Minute
	if len(history) > 4 {
		lastMsg := history[len(history)-1]
		if !lastMsg.Time.IsZero() && time.Since(lastMsg.Time) > staleGap {
			history = history[len(history)-4:]
		}
	}

	// Phase 2: Prefetch (network I/O — no engine state needed).
	var prefetchContent string
	var prefetchIDs []int64
	var prefetchSummaries string
	if db.Pool != nil && mc.cfg.EmbedURL != "" && strings.TrimSpace(msgText) != "" {
		pfCtx, pfCancel := context.WithTimeout(context.Background(), tools.TimeoutPrefetch)
		if pf := subconsciousPrefetch(pfCtx, db.Pool, mc.cfg.EmbedURL, mc.cfg.EmbedModel, msgText, history, excludePipelineID); pf != nil {
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

	// Phase 3.3: Inject recent system actions (routines, heartbeats) so the
	// orchestrator knows what the system recently did and avoids duplicate work.
	if xml := mc.eng.FormatRecentActionsXML(2 * mc.cfg.HeartbeatInterval); xml != "" {
		userPrompt += "\n\n" + xml
	}

	// Phase 3.4: Inject active background job context so the 9B can route
	// user messages to background Brain jobs via reply_to_job/cancel_job.
	if jobCtx := buildJobContext(mc.svc.StateMgr.GetJobs()); jobCtx != "" {
		userPrompt += "\n\n" + jobCtx
	}

	// Phase 3.5: Subagent dispatch for simple tool calls.
	dctx := dispatchContext{
		PersonalityContent: personalityContent,
		ProfileContent:     profileContent,
		PrefetchContent:    prefetchContent,
		TemporalCtx:        temporalCtx,
		PrefetchIDs:        prefetchIDs,
		PrefetchSummaries:  prefetchSummaries,
	}
	handled, escalation, dispatchAck := tryDispatch(mc, msg, chatID, msgText, dctx, history)
	if handled {
		typingCancel()
		return
	}
	// Only send an ack when there's a real escalation (failed dispatch that
	// the orchestrator needs to retry). Normal triage-to-orchestrator handoffs
	// don't need an ack — the 9B responds fast enough on its own.
	if escalation != nil && dispatchAck != "" {
		if _, ackErr := sendFormatted(mc.svc.Bot, chatID, msg.MessageID, formatReply(dispatchAck)); ackErr != nil {
			logger.Log.Warnf("[dispatch] ack send failed: %v", ackErr)
		} else {
			logger.Log.Debugf("[dispatch] ack sent for escalation: %q", textutil.Truncate(dispatchAck, 60))
		}
	}
	if escalation != nil {
		userPrompt += fmt.Sprintf("\n\n<escalation_context>\nA lightweight dispatch was attempted but failed.\nTool: %s\nPhase: %s\nError: %s\nDo NOT retry the same call with the same parameters. Try a different approach or explain the issue.\n</escalation_context>",
			escalation.ToolName, escalation.Phase, escalation.Error)
		if escalation.ToolResult != "" {
			userPrompt += fmt.Sprintf("\n\n<prior_tool_result tool=\"%s\">\nThe tool call succeeded but synthesis failed. Use these results directly — do NOT re-call the tool.\n%s\n</prior_tool_result>",
				escalation.ToolName, escalation.ToolResult)
		}
	}

	// Phase 4: Resolve which model handles this message. The 9B orchestrator
	// handles simple-to-moderate tasks directly (including tool calls); Brain
	// is the fallback when the 9B supervisor slot is busy.
	choice := mc.router.AcquireOrFallback(context.Background(), false)
	acquired := true
	defer func() {
		if acquired {
			choice.Release()
		}
	}()

	reply, msgs, err := llm.QueryOrchestrator(context.Background(), choice.Client, choice.Model, userPrompt, mc.confirmExec, mc.lb.TrimFn, &llm.QueryOrchestratorOpts{
		Parts:              visionParts,
		History:            history,
		PersonalityContent: personalityContent,
		ProfileContent:     profileContent,
		TemporalContext:    temporalCtx,
		PrefetchContent:    prefetchContent,
		MaxToolResultLen:   mc.cfg.MaxToolResultLen,
		MaxWebSources:      mc.cfg.MaxWebSources,
		ToolAgent:          mc.lb.ToolAgent,
		MandatedBrainTools: mandatedBrainTools,
		OnToolStart: func(toolName string) {
			choice.Release()
			acquired = false
			if toolName == "reason" {
				sendFormatted(mc.svc.Bot, chatID, msg.MessageID, formatReply("Thinking..."))
			}
		},
		OnToolEnd: func(reCtx context.Context) error {
			if reErr := choice.Reacquire(reCtx); reErr != nil {
				return reErr
			}
			acquired = true
			return nil
		},
	})

	// Check for BackgroundJobRequest — spawn a background Brain job.
	var bjr *llm.BackgroundJobRequest
	if errors.As(err, &bjr) {
		choice.Release()
		acquired = false
		typingCancel()

		userGoal := bjr.UserGoal
		if bjr.ProblemStatement != "" {
			userGoal = bjr.ProblemStatement
		}
		job := mc.svc.StateMgr.CreateJob(bjr.Tool, userGoal, chatID)
		job.TaskType = bjr.TaskType

		ack := brainSessionAcks[bjr.Tool]
		if ack == "" {
			ack = "Working on that in the background..."
		}
		sendFormatted(mc.svc.Bot, chatID, msg.MessageID, formatReply(ack))

		// Store the user message in conversation state.
		mc.svc.StateMgr.AppendMessage(llm.Message{Role: "user", Content: msgText})
		mc.svc.StateMgr.AppendMessage(llm.Message{Role: "assistant", Content: ack})

		go runBackgroundJob(mc, job)
		return
	}

	typingCancel()

	toolCtx, toolsUsed := summarizeToolContext(msgs)
	completeMessageHandling(mc, msg, chatID, messageResult{
		Reply:             reply,
		Messages:          condenseToolResults(msgs),
		ToolContext:       toolCtx,
		ToolsUsed:         toolsUsed,
		MsgText:           msgText,
		PrefetchIDs:       prefetchIDs,
		PrefetchSummaries: prefetchSummaries,
		PipelineID:        pipelineID,
		Err:               err,
	})
}

// ---------------------------------------------------------------------------
// Shared post-processing for both Brain and dispatch paths.
// ---------------------------------------------------------------------------

// messageResult normalizes the output of both the Brain and dispatch paths so
// that completeMessageHandling can apply identical post-processing.
type messageResult struct {
	Reply             string        // final text reply to send
	Messages          []llm.Message // condensed messages for state (Brain) or [user,assistant] pair (dispatch)
	ToolContext       string        // summarized tool usage for triage (Brain) or "[tool: X]\n" (dispatch)
	ToolsUsed         bool          // whether tools were called
	MsgText           string        // original user message text
	PrefetchIDs       []int64       // memory IDs from prefetch
	PrefetchSummaries string        // memory summaries from prefetch
	PipelineID        int64         // Telegram message ID for memory isolation
	Err               error         // LLM/execution error (nil on success)
}

// completeMessageHandling runs shared post-processing after both the Brain
// and dispatch paths: append to state, slide/archive, triage, memory
// usefulness evaluation, error fallback, and send reply.
func completeMessageHandling(mc messageContext, msg *tgbotapi.Message, chatID int64, mr messageResult) {
	// Append messages to conversation state.
	for _, m := range mr.Messages {
		mc.svc.StateMgr.AppendMessage(m)
	}

	// Slide and archive.
	if db.Pool != nil && mc.cfg.EmbedURL != "" {
		engine.SlideAndArchiveContext(context.Background(), mc.svc.StateMgr, mc.eng.MaxMessages, engine.ArchiveDeps{
			DB: db.Pool, EmbedEndpoint: mc.cfg.EmbedURL, EmbedModel: mc.cfg.EmbedModel,
			DTCQueueFn: mc.svc.DTCQueueFunc, SubagentFn: mc.svc.SubagentFunc,
			GrammarFn: mc.svc.GrammarFunc, BgGrammarFn: mc.svc.BgGrammarFunc, QueueFn: mc.svc.QueueFunc,
			PipelineID: mr.PipelineID,
		})
	}

	// Triage and save conversation.
	if mr.Err == nil && db.Pool != nil && mc.cfg.EmbedURL != "" && mc.svc.DTC != nil && mc.emailTriageCfg != nil {
		exchange := mr.ToolContext + fmt.Sprintf("user: %s\nassistant: %s", mr.MsgText, mr.Reply)
		pipelines.TriageAndSaveConversationAsync(*mc.emailTriageCfg, exchange, mr.ToolsUsed, mr.PipelineID)
	}

	// Record pipeline ID so the next message's prefetch can exclude stale memories.
	if mr.PipelineID != 0 {
		mc.svc.StateMgr.SetLastPipelineID(mr.PipelineID)
	}

	// Memory usefulness evaluation.
	if mr.Err == nil && len(mr.PrefetchIDs) > 0 && db.Pool != nil && mc.svc.Subagent != nil {
		capturedIDs := mr.PrefetchIDs
		capturedReply := mr.Reply
		capturedMsgText := mr.MsgText
		capturedSubagent := mc.svc.Subagent
		capturedSummaries := mr.PrefetchSummaries
		go evaluateMemoryUsefulnessViaSubagent(db.Pool, capturedSubagent, capturedIDs, capturedMsgText, capturedReply, capturedSummaries)
	}

	reply := mr.Reply
	if mr.Err != nil {
		logger.Log.Errorf("LLM error: %v", mr.Err)
		reply = "Sorry, something went wrong processing your message."
	}

	// Don't send orchestrator control tags to the user.
	if strings.Contains(reply, "<NO_ACTION_REQUIRED>") {
		return
	}

	// Format and send reply.
	fm := formatReply(reply)
	if _, err := sendFormatted(mc.svc.Bot, chatID, msg.MessageID, fm); err != nil {
		logger.Log.Warnf("Entity send failed, falling back to plain text: %v", err)
		replyMsg := tgbotapi.NewMessage(chatID, reply)
		replyMsg.ReplyToMessageID = msg.MessageID
		if _, err := mc.svc.Bot.Send(replyMsg); err != nil {
			logger.Log.Errorf("Error sending message: %v", err)
		}
	}

	// Emit curiosity signal when the orchestrator expressed uncertainty.
	emitConversationGapSignal(mc.eng, reply)
}

// uncertaintyPatterns matches replies where the orchestrator expressed a knowledge gap.
var uncertaintyPatterns = regexp.MustCompile(`(?i)(I don't have (?:enough )?information|I'm not sure|I couldn't find|I don't know|I wasn't able to find|I lack information)`)

// emitConversationGapSignal checks if the orchestrator's reply indicates
// uncertainty and emits a curiosity signal for background research.
func emitConversationGapSignal(eng *engine.Engine, reply string) {
	if eng.CuriositySignals == nil || len(reply) < 20 {
		return
	}
	match := uncertaintyPatterns.FindString(reply)
	if match == "" {
		return
	}
	// Extract a topic hint from the reply (first 120 chars after the match).
	idx := strings.Index(reply, match)
	topic := reply
	if idx >= 0 {
		end := idx + len(match) + 120
		if end > len(reply) {
			end = len(reply)
		}
		topic = reply[idx:end]
	}
	select {
	case eng.CuriositySignals <- engine.CuriositySignal{
		Source:   "conversation",
		Query:    topic,
		Priority: 5,
	}:
		logger.Log.Debugf("[curiosity-signal] conversation gap detected: %s", textutil.Truncate(topic, 80))
	default:
		// Channel full, drop.
	}
}

