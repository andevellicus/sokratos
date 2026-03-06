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
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/pipelines"
	"sokratos/platform"
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
	platform       platform.Platform
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
		registerGmailTools(mc.registry, db.Pool, mc.emailTriageCfg, mc.cfg.EmailDisplayBatch, mc.svc.Subagent)
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
	// conversation message, old topics are stale and will confuse the Brain.
	// Keep only recent messages for immediate context.
	const staleGap = 30 * time.Minute
	if len(history) > 4 {
		lastMsg := history[len(history)-1]
		if !lastMsg.Time.IsZero() && time.Since(lastMsg.Time) > staleGap {
			history = history[len(history)-4:]
		}
	}

	// Phase 2: Start prefetch + temporal context in a background goroutine.
	// These run concurrently with triage (Phase 3.5), which doesn't need
	// prefetch results — it only uses msgText + history.
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

	// Phase 3.5: Check supervisor slot availability. If free, skip triage
	// and go straight to the orchestrator — saves 3-5s of triage latency.
	// Triage is only valuable when the supervisor is busy: it dispatches
	// simple tool calls on a subagent slot without waiting.
	var triageResult *dispatchResult
	choice, gotSupervisor := mc.router.TryAcquirePrimary()
	if !gotSupervisor {
		// Supervisor busy — run triage (concurrent with prefetch) as fast-path.
		triageResult, _ = runTriage(mc, msgText, history)
	}

	// Phase 3.6: Wait for prefetch (usually already done — prefetch ~2s,
	// triage ~3-5s, so prefetch finishes while triage is running).
	pf := <-pfCh

	// Build dispatch context with prefetch results.
	dctx := dispatchContext{
		PersonalityContent: personalityContent,
		ProfileContent:     profileContent,
		PrefetchContent:    pf.content,
		TemporalCtx:        pf.temporal,
		PrefetchIDs:        pf.ids,
		PrefetchSummaries:  pf.summaries,
	}

	// Phase 3.7: Execute dispatch decision (needs prefetch for synthesis).
	if triageResult != nil && triageResult.Dispatch {
		handled, escalation := executeDispatch(mc, msg, msgText, dctx, triageResult)
		if handled {
			mc.svc.Metrics.Since("message.total", messageStart, map[string]string{"path": "dispatch"})
			if gotSupervisor {
				choice.Release()
			}
			typingCancel()
			return
		}
		if escalation != nil {
			userPrompt += fmt.Sprintf("\n\n<escalation_context>\nA lightweight dispatch was attempted but failed.\nTool: %s\nPhase: %s\nError: %s\nDo NOT retry the same call with the same parameters. Try a different approach or explain the issue.\n</escalation_context>",
				escalation.ToolName, escalation.Phase, escalation.Error)
			if escalation.ToolResult != "" {
				userPrompt += fmt.Sprintf("\n\n<prior_tool_result tool=\"%s\">\nThe tool call succeeded but synthesis failed. Use these results directly — do NOT re-call the tool.\n%s\n</prior_tool_result>",
					escalation.ToolName, escalation.ToolResult)
			}
		}
	}

	// Phase 4: Acquire orchestrator slot if we don't already have one.
	if !gotSupervisor {
		choice = mc.router.AcquireOrFallback(context.Background(), false, engine.PriorityUser)
	}
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
		TemporalContext:    pf.temporal,
		PrefetchContent:    pf.content,
		MaxToolResultLen:   mc.cfg.MaxToolResultLen,
		MaxWebSources:      mc.cfg.MaxWebSources,
		ToolAgent:          mc.lb.ToolAgent,
		MandatedBrainTools: mandatedBrainTools,
		EscalateTools:      escalateTools,
		OnToolStart: func(toolName string) {
			choice.ReleaseReserved()
			acquired = false
			if toolName == "reason" {
				mc.platform.Send(context.Background(), msg.ChannelID, "Thinking...", msg.ID)
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
	})

	// Check for EscalationRequest — replay on Brain inline.
	var esc *llm.EscalationRequest
	if errors.As(err, &esc) {
		logger.Log.Infof("[escalation] %s triggered escalation to Brain", esc.ToolName)
		mc.platform.Send(context.Background(), msg.ChannelID, "Thinking...", msg.ID)
		choice.Release()
		acquired = false

		// Acquire Brain (preferBrain=true) at user priority.
		choice = mc.router.AcquireOrFallback(context.Background(), true, engine.PriorityUser)
		acquired = true

		// Replay on Brain without EscalateTools (Brain handles everything directly).
		reply, msgs, err = llm.QueryOrchestrator(context.Background(), choice.Client, choice.Model, userPrompt, mc.confirmExec, mc.lb.TrimFn, &llm.QueryOrchestratorOpts{
			Parts:              visionParts,
			History:            history,
			PersonalityContent: personalityContent,
			ProfileContent:     profileContent,
			TemporalContext:    pf.temporal,
			PrefetchContent:    pf.content,
			MaxToolResultLen:   mc.cfg.MaxToolResultLen,
			MaxWebSources:      mc.cfg.MaxWebSources,
			ToolAgent:          mc.lb.ToolAgent,
			OnToolStart: func(toolName string) {
				choice.ReleaseReserved()
				acquired = false
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
		})
		mc.svc.Metrics.Since("message.total", messageStart, map[string]string{"path": "escalated"})
	}

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

	msgPath := "brain"
	if gotSupervisor {
		msgPath = "direct"
	}
	mc.svc.Metrics.Since("message.total", messageStart, map[string]string{"path": msgPath})

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
	PipelineID        int64         // message ID for memory isolation
	Err               error         // LLM/execution error (nil on success)
}

// completeMessageHandling runs shared post-processing after both the Brain
// and dispatch paths: append to state, slide/archive, triage, memory
// usefulness evaluation, error fallback, and send reply.
func completeMessageHandling(mc messageContext, msg *platform.IncomingMessage, mr messageResult) {
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
