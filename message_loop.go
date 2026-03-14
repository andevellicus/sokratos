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

	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/google"
	"sokratos/grammar"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/orchestrate"
	"sokratos/pipelines"
	"sokratos/platform"
	"sokratos/prompts"
	"sokratos/routines"
	"sokratos/textutil"
	"sokratos/timeouts"
	"sokratos/toolreg"
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
	triageCfg      *pipelines.TriageConfig
	confirmExec    func(context.Context, json.RawMessage) (string, error)
	skillMtimes    map[string]time.Time
	skillDeps      tools.SkillDeps
	rebuildGrammar func()
	router         engine.SlotRouter
	platform       platform.Platform
	selector       *toolreg.ToolSelector // nil = use full tool set (no dynamic selection)
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

// handleMetrics runs a pre-built metrics report. Accepts optional args:
// "/metrics" (overview, 1h), "/metrics slots", "/metrics dispatch 24h".
func handleMetrics(_ messageContext, args string) string {
	parts := strings.Fields(args)
	var report, window string
	if len(parts) >= 1 {
		report = parts[0]
	}
	if len(parts) >= 2 {
		window = parts[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
	defer cancel()
	result, err := tools.QueryMetricsReport(ctx, db.Pool, report, window)
	if err != nil {
		return "Metrics query failed: " + err.Error()
	}
	return result
}

// handleBootstrap launches a profile generation run in the background.
// Returns an immediate acknowledgement string.
func handleBootstrap(mc messageContext) string {
	if db.Pool == nil || mc.svc.DTC == nil || mc.cfg.EmbedURL == "" {
		return "Bootstrap requires database, deep thinker, and embedding service."
	}
	bootstrapSend := func(text string) {
		mc.platform.Broadcast(context.Background(), text)
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

// handleGoogle triggers Google OAuth re-authentication via the platform.
// Uses a single OAuth flow with combined Gmail+Calendar scopes so only
// one auth URL + code paste is needed. Re-initializes both services and
// registers tools that were previously disabled.
func handleGoogle(mc messageContext) string {
	gmailWasNil := google.GmailService == nil
	calWasNil := google.CalendarService == nil

	// Delete existing token to force a fresh OAuth flow.
	os.Remove(mc.cfg.GoogleTokenPath)

	// Build auth IO that reads replies from the platform.
	authIO := &google.AuthIO{
		Send: func(msg string) {
			mc.platform.Broadcast(context.Background(), msg)
		},
		Receive: func() (string, error) {
			return mc.platform.ReadReply()
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
		registerGmailTools(mc.registry, db.Pool, mc.triageCfg, mc.cfg.EmailDisplayBatch, mc.svc.Subagent)
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
func processMessage(mc messageContext, msg *platform.IncomingMessage, msgText, userPrompt string, visionParts []llm.ContentPart) {
	messageStart := time.Now()
	typingCancel := mc.platform.StartTyping(context.Background(), msg.ChannelID)

	// Phase 0: Pipeline isolation — tag this message and exclude prior pipeline's memories.
	pipelineID := msg.PipelineID()
	excludePipelineID := mc.svc.StateMgr.LastPipelineID()

	// Phase 1: Snapshot history (StateManager has its own RWMutex).
	history := mc.svc.StateMgr.ReadMessages()

	// Phase 1.5: Staleness trim — if there's a long gap since the last
	// conversation message, old topics are stale and will confuse the model.
	// Keep only recent messages for immediate context.
	const staleGap = 30 * time.Minute
	if len(history) > 4 {
		lastMsg := history[len(history)-1]
		if !lastMsg.Time.IsZero() && time.Since(lastMsg.Time) > staleGap {
			history = history[len(history)-4:]
		}
	}

	// Phase 2: Start prefetch + temporal context + tool selection in parallel.
	type prefetchData struct {
		content   string
		ids       []int64
		summaries string
		temporal  string
	}
	pfCh := make(chan prefetchData, 1)
	go func() {
		pfStart := time.Now()
		var pd prefetchData
		var memoriesFound int
		if db.Pool != nil && mc.cfg.EmbedURL != "" && strings.TrimSpace(msgText) != "" {
			pfCtx, pfCancel := context.WithTimeout(context.Background(), tools.TimeoutPrefetch)
			if pf := subconsciousPrefetch(pfCtx, db.Pool, mc.cfg.EmbedURL, mc.cfg.EmbedModel, msgText, history, excludePipelineID); pf != nil {
				pd.content = pf.Summaries
				pd.ids = pf.IDs
				pd.summaries = pf.Summaries
				memoriesFound = len(pf.IDs)
			}
			pfCancel()
		}
		if db.Pool != nil {
			pd.temporal = engine.BuildTemporalContext(context.Background(), db.Pool)
		}
		mc.svc.Metrics.Since("prefetch.duration", pfStart, map[string]string{
			"memories_found": fmt.Sprintf("%d", memoriesFound),
		})
		pfCh <- pd
	}()

	// Tool selection: embed query and select relevant tools (parallel with prefetch).
	type toolSelectionData struct {
		agent *llm.ToolAgentConfig // nil = use full tool set
	}
	tsCh := make(chan toolSelectionData, 1)
	go func() {
		if mc.selector == nil {
			tsCh <- toolSelectionData{}
			return
		}
		tsCtx, tsCancel := context.WithTimeout(context.Background(), timeouts.Embedding)
		defer tsCancel()
		names, err := mc.selector.Select(tsCtx, msgText)
		if err != nil {
			logger.Log.Warnf("[tool-selector] selection failed, using full set: %v", err)
			tsCh <- toolSelectionData{}
			return
		}
		if names == nil {
			tsCh <- toolSelectionData{}
			return
		}
		// Build per-request tool descriptions and grammar.
		toolIndex := toolreg.BuildSelectedToolIndex(mc.registry, names)
		td := strings.Replace(prompts.Tools, "%TOOL_INDEX%", toolIndex, 1)
		schemas := mc.registry.SchemasForTools(names)
		grammarStr := grammar.BuildSubagentToolGrammar(schemas)
		tsCh <- toolSelectionData{
			agent: &llm.ToolAgentConfig{
				ToolDescriptions: td,
				Grammar:          grammarStr,
			},
		}
	}()

	// Phase 3: Snapshot personality/profile under the lock (microseconds),
	// then release before the multi-second inference call.
	mc.eng.Mu.Lock()
	personalityContent := mc.eng.PersonalityContent
	profileContent := mc.eng.ProfileContent
	mc.eng.Mu.Unlock()

	// Phase 3.3: Inject recent system actions (routines, heartbeats) so the
	// orchestrator knows what the system recently did and avoids duplicate work.
	if xml := mc.eng.FormatRecentActionsXML(2 * mc.cfg.HeartbeatInterval); xml != "" {
		userPrompt += "\n\n" + xml
	}

	// Phase 3.4: Inject active background job context so the orchestrator can
	// route user messages to background Brain jobs via reply_to_job/cancel_job.
	if jobCtx := buildJobContext(mc.svc.StateMgr.GetJobs()); jobCtx != "" {
		userPrompt += "\n\n" + jobCtx
	}

	// Phase 3.5: Wait for prefetch and tool selection results.
	pf := <-pfCh
	ts := <-tsCh

	// Phase 4: Acquire orchestrator slot.
	// Prefer 9B for all orchestration — it's the fast grammar-constrained
	// supervisor. If it needs deep reasoning, it can call deep_think/consult_deep_thinker.
	choice := mc.router.AcquireOrFallback(context.Background(), false, engine.PriorityUser)
	acquired := true
	defer func() {
		if acquired {
			choice.Release()
		}
	}()

	// Progress handle for the orchestrator — created lazily on the
	// first tool call so messages without tools don't get a stale progress msg.
	var ph *platform.ProgressHandle

	// Wrap confirmExec to inject progress reporting into the tool context.
	progressExec := func(ctx context.Context, raw json.RawMessage) (string, error) {
		if ph != nil {
			ctx = tools.WithProgress(ctx, func(status string) {
				ph.Update(context.Background(), status)
			})
		}
		return mc.confirmExec(ctx, raw)
	}

	// Use per-request tool selection if available, otherwise fall back to full set.
	toolAgent := mc.lb.ToolAgent
	if ts.agent != nil {
		toolAgent = ts.agent
	}

	opts := llm.QueryOrchestratorOpts{
		Parts:              visionParts,
		History:            history,
		PersonalityContent: personalityContent,
		ProfileContent:     profileContent,
		TemporalContext:    pf.temporal,
		PrefetchContent:    pf.content,
		MaxToolResultLen:   mc.cfg.MaxToolResultLen,
		MaxWebSources:      mc.cfg.MaxWebSources,
		ToolAgent:          toolAgent,
		MandatedBrainTools: mandatedBrainTools,
		FirstRoundThinking: true, // think on round 0 to improve tool routing decisions
		OnToolStart: func(toolName string) {
			choice.ReleaseReserved()
			acquired = false
			// Create or update progress handle with the tool being called.
			status := mc.registry.GetProgressLabel(toolName)
			if ph == nil {
				handle, err := platform.NewProgressHandle(context.Background(), mc.platform, msg.ChannelID, status, msg.ID)
				if err == nil {
					ph = handle
				}
			} else {
				ph.Update(context.Background(), status)
			}
		},
		OnToolEnd: func(reCtx context.Context) error {
			if reErr := choice.Reacquire(reCtx); reErr != nil {
				return reErr
			}
			acquired = true
			return nil
		},
		OnToolExec: func(tool string, dur time.Duration, toolErr error) {
			result := "ok"
			if toolErr != nil {
				result = "hard_error"
			}
			mc.svc.Metrics.EmitDuration("tool.exec", dur, map[string]string{"tool": tool, "result": result})
		},
	}

	reply, msgs, err := llm.QueryOrchestrator(context.Background(), choice.Client, choice.Model, userPrompt, progressExec, mc.lb.TrimFn, &opts)

	// Check for BackgroundJobRequest — spawn a background Brain job.
	var bjr *orchestrate.BackgroundJobRequest
	if errors.As(err, &bjr) {
		choice.Release()
		acquired = false
		typingCancel()

		userGoal := bjr.UserGoal
		if bjr.ProblemStatement != "" {
			userGoal = bjr.ProblemStatement
		}
		job := mc.svc.StateMgr.CreateJob(bjr.Tool, userGoal, msg.ChannelID)
		job.TaskType = bjr.TaskType

		ack := brainSessionAcks[bjr.Tool]
		if ack == "" {
			ack = "Working on that in the background..."
		}
		mc.platform.Send(context.Background(), msg.ChannelID, ack, msg.ID)

		// Store the user message in conversation state.
		mc.svc.StateMgr.AppendMessage(llm.Message{Role: "user", Content: msgText})
		mc.svc.StateMgr.AppendMessage(llm.Message{Role: "assistant", Content: ack})

		go runBackgroundJob(mc, job)
		return
	}

	typingCancel()

	mc.svc.Metrics.Since("message.total", messageStart, map[string]string{"path": "orchestrator"})

	toolCtx, toolsUsed := summarizeToolContext(msgs)
	completeMessageHandling(mc, msg, messageResult{
		Reply:             reply,
		Messages:          condenseToolResults(msgs),
		ToolContext:       toolCtx,
		ToolsUsed:         toolsUsed,
		MsgText:           msgText,
		PrefetchIDs:       pf.ids,
		PrefetchSummaries: pf.summaries,
		PipelineID:        pipelineID,
		Err:               err,
	})
}

// ---------------------------------------------------------------------------
// Shared post-processing for both Brain and dispatch paths.
// ---------------------------------------------------------------------------

// messageResult normalizes the output of the orchestrator path so that
// completeMessageHandling can apply identical post-processing.
type messageResult struct {
	Reply             string        // final text reply to send
	Messages          []llm.Message // condensed messages for state
	ToolContext       string        // summarized tool usage for triage
	ToolsUsed         bool          // whether tools were called
	MsgText           string        // original user message text
	PrefetchIDs       []int64       // memory IDs from prefetch
	PrefetchSummaries string        // memory summaries from prefetch
	PipelineID        int64         // message ID for memory isolation
	Err               error         // LLM/execution error (nil on success)
}

// completeMessageHandling runs shared post-processing after the orchestrator:
// append to state, slide/archive, triage, memory usefulness evaluation,
// error fallback, and send reply.
func completeMessageHandling(mc messageContext, msg *platform.IncomingMessage, mr messageResult) {
	// Append messages to conversation state.
	for _, m := range mr.Messages {
		mc.svc.StateMgr.AppendMessage(m)
	}

	// Slide and archive.
	if db.Pool != nil && mc.cfg.EmbedURL != "" {
		engine.SlideAndArchiveContext(context.Background(), mc.svc.StateMgr, mc.eng.MaxMessages, engine.ArchiveDeps{
			DB: db.Pool, EmbedEndpoint: mc.cfg.EmbedURL, EmbedModel: mc.cfg.EmbedModel,
			MemoryFuncs: engine.MemoryFuncs{
				DTCQueueFn: mc.svc.DTCQueueFunc, SubagentFn: mc.svc.SubagentFunc,
				GrammarFn: mc.svc.GrammarFunc, BgGrammarFn: mc.svc.BgGrammarFunc, QueueFn: mc.svc.QueueFunc,
			},
			PipelineID: mr.PipelineID,
		})
	}

	// Triage and save conversation.
	if mr.Err == nil && db.Pool != nil && mc.cfg.EmbedURL != "" && mc.svc.DTC != nil && mc.triageCfg != nil {
		exchange := mr.ToolContext + fmt.Sprintf("user: %s\nassistant: %s", mr.MsgText, mr.Reply)
		pipelines.TriageAndSaveConversationAsync(*mc.triageCfg, exchange, mr.ToolsUsed, mr.PipelineID)
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

	// Send reply via platform (handles formatting + fallback internally).
	if _, err := mc.platform.Send(context.Background(), msg.ChannelID, reply, msg.ID); err != nil {
		logger.Log.Errorf("Error sending message: %v", err)
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
