package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"sokratos/clients"
	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/google"
	"sokratos/grammar"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/pipelines"
	"sokratos/routines"
	"sokratos/textutil"
	"sokratos/timefmt"
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
	skillDeps      tools.SkillDeps
	rebuildGrammar func()
	router         engine.SlotRouter
	delegateConfig *tools.DelegateConfig
	messageChan    <-chan *tgbotapi.Message // for /google auth code reads
}

// dispatchContext bundles the context strings and prefetch metadata that are
// threaded through tryDispatch, tryMultiStepDispatch, and prompt builders.
type dispatchContext struct {
	PersonalityContent string
	ProfileContent     string
	PrefetchContent    string
	TemporalCtx        string
	PrefetchIDs        []int64
	PrefetchSummaries  string
}

// handleReload forces a full re-sync of routines.toml and skills from disk.
// Returns a human-readable summary of what changed.
func handleReload(mc messageContext) string {
	added, updated, deleted := routines.SyncFromFile(db.Pool, "routines.toml")
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
		QueueFn:     mc.svc.QueueFunc,
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

	// Phase 3.3: Inject recent system actions (routines, heartbeats) so the
	// orchestrator knows what the system recently did and avoids duplicate work.
	if xml := mc.eng.FormatRecentActionsXML(2 * mc.cfg.HeartbeatInterval); xml != "" {
		userPrompt += "\n\n" + xml
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
	handled, escalation, dispatchAck := tryDispatch(mc, msg, chatID, msgText, userPrompt, dctx, history)
	if handled {
		typingCancel()
		return
	}
	// Send 9B ack to user immediately (latency win).
	if dispatchAck != "" {
		sendFormatted(mc.svc.Bot, chatID, msg.MessageID, formatReply(dispatchAck))
	}
	if escalation != nil {
		userPrompt += fmt.Sprintf("\n\n<escalation_context>\nA lightweight dispatch was attempted but failed.\nTool: %s\nPhase: %s\nError: %s\nDo NOT retry the same call with the same parameters. Try a different approach or explain the issue.\n</escalation_context>",
			escalation.ToolName, escalation.Phase, escalation.Error)
		if escalation.ToolResult != "" {
			userPrompt += fmt.Sprintf("\n\n<prior_tool_result tool=\"%s\">\nThe tool call succeeded but synthesis failed. Use these results directly — do NOT re-call the tool.\n%s\n</prior_tool_result>",
				escalation.ToolName, escalation.ToolResult)
		}
	}

	// Phase 4: Resolve which model handles this message (Brain preferred for interactive accuracy).
	choice := mc.router.AcquireOrFallback(context.Background(), true)
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
		Fallbacks:          mc.fallbacks,
		OnToolStart:        func() { choice.Release(); acquired = false },
		OnToolEnd: func(reCtx context.Context) error {
			if reErr := choice.Reacquire(reCtx); reErr != nil {
				return reErr
			}
			acquired = true
			return nil
		},
	})
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
		})
	}

	// Triage and save conversation.
	if mr.Err == nil && db.Pool != nil && mc.cfg.EmbedURL != "" && mc.svc.DTC != nil && mc.emailTriageCfg != nil {
		exchange := mr.ToolContext + fmt.Sprintf("user: %s\nassistant: %s", mr.MsgText, mr.Reply)
		pipelines.TriageAndSaveConversationAsync(*mc.emailTriageCfg, exchange, mr.ToolsUsed)
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
}

// ---------------------------------------------------------------------------
// Subagent dispatch: lightweight triage that routes simple tool calls around
// the Brain entirely. See plan "Subagent Triage — Route Simple Tool Calls
// Around the Brain" for design rationale.
// ---------------------------------------------------------------------------

// dispatchResult is the parsed output from the triage grammar.
type dispatchResult struct {
	Dispatch  bool            `json:"dispatch"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Multi     bool            `json:"multi"`
	Directive string          `json:"directive"`
	Ack       string          `json:"ack"`
}

const (
	timeoutDispatchTriage       = 5 * time.Second
	timeoutDispatchToolExec     = 5 * time.Minute
	timeoutDispatchSynthesis    = 30 * time.Second
	timeoutDispatchDTCSynthesis = 45 * time.Second
	dispatchMaxTriageTokens     = 512
	dispatchMaxSynthTokens      = 2048
	dispatchMaxResultLen        = 8000
)

// dispatchEscalation captures context from a failed dispatch attempt so
// the Brain can avoid repeating the same failing call.
type dispatchEscalation struct {
	ToolName   string // empty if triage itself failed
	Error      string // error description
	Phase      string // "triage" | "execution" | "synthesis" | "multi-step"
	ToolResult string // truncated successful tool result (non-empty when synthesis failed after tool succeeded)
}

// dispatchProgressInterval is how often to send a "still working..." update
// to the user during long-running tool execution in the dispatch path.
const dispatchProgressInterval = 20 * time.Second

// tryDispatch attempts to handle a user message via the subagent dispatch path
// (triage → tool call → synthesis) without involving the Brain. Returns
// (handled, escalation, ack). handled=true means fully handled. ack is the
// triage-generated acknowledgement text for escalations (dispatch:false).
func tryDispatch(mc messageContext, msg *tgbotapi.Message, chatID int64,
	msgText, userPrompt string, dctx dispatchContext, history []llm.Message) (bool, *dispatchEscalation, string) {

	if mc.svc.Subagent == nil {
		return false, nil, ""
	}

	// --- Triage ---
	used, total := mc.svc.Subagent.SlotsInUse()
	logger.Log.Debugf("[dispatch] triage starting (subagent slots: %d/%d used)", used, total)

	triagePrompt := buildTriageSystemPrompt(mc.registry, timefmt.FormatNatural(time.Now()))
	triageInput := buildTriageInput(msgText, history)

	triageCtx, triageCancel := context.WithTimeout(context.Background(), timeoutDispatchTriage)
	raw, err := mc.svc.Subagent.TryCompleteWithGrammar(triageCtx, triagePrompt, triageInput, grammar.BuildDispatchGrammar(), dispatchMaxTriageTokens)
	triageCancel()
	if err != nil {
		logger.Log.Debugf("[dispatch] triage skipped (subagent slots: %d/%d): %v", used, total, err)
		return false, nil, "" // slots busy — clean escalation
	}

	var dr dispatchResult
	if err := json.Unmarshal([]byte(raw), &dr); err != nil {
		logger.Log.Warnf("[dispatch] triage parse failed: %v — raw: %s", err, textutil.Truncate(raw, 200))
		return false, nil, ""
	}
	if !dr.Dispatch {
		logger.Log.Debug("[dispatch] triage decided to escalate")
		return false, nil, dr.Ack
	}

	// --- Multi-step dispatch via SubagentSupervisor ---
	if dr.Multi {
		return tryMultiStepDispatch(mc, msg, chatID, msgText, dctx, dr.Directive)
	}

	if !mc.registry.Has(dr.Tool) {
		logger.Log.Warnf("[dispatch] triage returned unknown tool %q, escalating", dr.Tool)
		return false, nil, ""
	}

	used, total = mc.svc.Subagent.SlotsInUse()
	logger.Log.Infof("[dispatch] dispatching %s (subagent slots: %d/%d used)", dr.Tool, used, total)

	// Send LLM-generated ack if the triage model wrote one.
	if dr.Ack != "" {
		ackFm := formatReply(dr.Ack)
		sendFormatted(mc.svc.Bot, chatID, msg.MessageID, ackFm)
	}

	// --- Execute tool with periodic progress updates ---
	toolCall := tools.ToolCall{Name: dr.Tool, Arguments: dr.Args}
	toolJSON, _ := json.Marshal(toolCall)

	// Progress ticker: sends periodic updates so the user knows it's still alive.
	progressCtx, progressCancel := context.WithCancel(context.Background())
	defer progressCancel()
	toolStart := time.Now()
	go func() {
		ticker := time.NewTicker(dispatchProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				elapsed := int(time.Since(toolStart).Seconds())
				update := fmt.Sprintf("Still working on %s... (%ds)", dr.Tool, elapsed)
				sendFormatted(mc.svc.Bot, chatID, msg.MessageID, formatReply(update))
				logger.Log.Debugf("[dispatch] progress: %s running for %ds", dr.Tool, elapsed)
			case <-progressCtx.Done():
				return
			}
		}
	}()

	toolCtx, toolCancel := context.WithTimeout(context.Background(), timeoutDispatchToolExec)
	result, execErr := mc.registry.Execute(toolCtx, toolJSON)
	toolCancel()
	progressCancel() // stop progress ticker

	elapsed := time.Since(toolStart)
	logger.Log.Infof("[dispatch] %s completed in %s", dr.Tool, elapsed.Round(time.Millisecond))

	if execErr != nil {
		logger.Log.Warnf("[dispatch] tool %s hard error: %v — escalating", dr.Tool, execErr)
		return false, &dispatchEscalation{ToolName: dr.Tool, Error: execErr.Error(), Phase: "execution"}, ""
	}
	if llm.IsToolSoftError(result) {
		logger.Log.Infof("[dispatch] tool %s soft error, escalating to Brain for recovery", dr.Tool)
		return false, &dispatchEscalation{ToolName: dr.Tool, Error: result, Phase: "execution"}, ""
	}

	// --- Synthesize ---
	logger.Log.Debugf("[dispatch] synthesizing response for %s (%d chars of result)", dr.Tool, len(result))
	synthesisPrompt := buildSynthesisPrompt(dctx)
	truncatedResult := result
	if len(truncatedResult) > dispatchMaxResultLen {
		truncatedResult = truncatedResult[:dispatchMaxResultLen] + "\n... (truncated)"
	}
	synthesisInput := fmt.Sprintf("The user said: %s\n\nHere's what came back:\n%s", msgText, truncatedResult)

	synthCtx, synthCancel := context.WithTimeout(context.Background(), timeoutDispatchSynthesis)
	reply, synthErr := mc.svc.Subagent.Complete(synthCtx, synthesisPrompt, synthesisInput, dispatchMaxSynthTokens)
	synthCancel()

	if synthErr != nil {
		// Tier 2: Try DTC CompleteNoThink as lightweight synthesis fallback.
		if mc.svc.DTC != nil {
			logger.Log.Infof("[dispatch] subagent synthesis failed, trying DTC fallback for %s", dr.Tool)
			dtcCtx, dtcCancel := context.WithTimeout(context.Background(), timeoutDispatchDTCSynthesis)
			reply, synthErr = mc.svc.DTC.CompleteNoThink(dtcCtx, synthesisPrompt, synthesisInput, dispatchMaxSynthTokens)
			dtcCancel()
		}
		// Tier 3: Both subagent and DTC failed — escalate to Brain with tool result attached.
		if synthErr != nil {
			logger.Log.Warnf("[dispatch] all synthesis tiers failed for %s, escalating to Brain with tool result", dr.Tool)
			return false, &dispatchEscalation{
				ToolName:   dr.Tool,
				Error:      synthErr.Error(),
				Phase:      "synthesis",
				ToolResult: truncatedResult,
			}, ""
		}
	}
	reply = textutil.StripThinkTags(reply)

	// --- Post-processing + send (shared with Brain path) ---
	completeMessageHandling(mc, msg, chatID, messageResult{
		Reply: reply,
		Messages: []llm.Message{
			{Role: "user", Content: msgText},
			{Role: "assistant", Content: reply},
		},
		ToolContext:       fmt.Sprintf("[tool: %s]\n", dr.Tool),
		ToolsUsed:         true,
		MsgText:           msgText,
		PrefetchIDs:       dctx.PrefetchIDs,
		PrefetchSummaries: dctx.PrefetchSummaries,
	})

	totalElapsed := time.Since(toolStart)
	logger.Log.Infof("[dispatch] handled %q via %s in %s (subagent path)", textutil.Truncate(msgText, 60), dr.Tool, totalElapsed.Round(time.Millisecond))
	return true, nil, ""
}

// buildTriageSystemPrompt constructs the system prompt for dispatch triage.
// It includes a compact tool index and skill descriptions so the subagent
// knows which tools exist and can pick one.
func buildTriageSystemPrompt(registry *tools.Registry, currentTime string) string {
	var sb strings.Builder
	sb.WriteString(`You are a dispatch triage agent. Decide whether the user's message can be handled by dispatching to tools directly, or whether it needs the full orchestrator (Brain).

Output JSON matching the grammar:
- Single tool: {"dispatch": true, "tool": "<name>", "args": {<arguments>}, "ack": "<brief natural reply>"}
- Multi-step: {"dispatch": true, "multi": true, "directive": "<natural language instruction>", "ack": "<brief natural reply>"}
- Escalate: {"dispatch": false, "ack": "<brief natural reply>"}

## Rules
1. DISPATCH (single tool) when the request is a straightforward data fetch that one tool can answer directly.
2. DISPATCH (multi-step) when the request needs 2-3 sequential tool calls but no complex reasoning (e.g., "search for X and read the top result", "check my calendar and email"). Write the directive as a clear instruction describing what to do.
3. ESCALATE when:
   - The request needs judgment, creativity, or complex multi-step reasoning
   - The request is conversational (greetings, opinions, advice, jokes)
   - The request involves side effects (sending emails, creating events, managing skills/routines)
   - You are unsure — escalation is always safe
4. NEVER dispatch: send_email, create_event, create_skill, manage_skills, manage_routines, manage_personality, save_memory, forget_topic, consult_deep_thinker, plan_and_execute, delegate_task, ask_database
5. Only include arguments the tool actually needs. Omit optional parameters when the user doesn't specify them — pass {} for default behavior. Never invent placeholder values like "all" or "any".
6. Read the conversation history carefully. Choose the tool that best matches the user's ACTUAL intent — a follow-up question about a specific topic is different from the original broad request.
7. The "ack" field is a brief, natural reply shown to the user while the tool runs. Keep it short and conversational (e.g. "Sure, let me check." or "One sec."). Do NOT describe the tool or its parameters.

Current time: `)
	sb.WriteString(currentTime)
	sb.WriteString("\n\n## Available Tools\n")
	sb.WriteString(registry.CompactIndex())

	if skills := registry.DynamicSkillDescriptions(); skills != "" {
		sb.WriteString("\n")
		sb.WriteString(skills)
	}

	return sb.String()
}

// buildTriageInput constructs the user message for the triage call, including
// a snippet of recent conversation history for context.
func buildTriageInput(msgText string, history []llm.Message) string {
	var sb strings.Builder

	// Include up to 4 recent history messages for conversational context.
	start := 0
	if len(history) > 4 {
		start = len(history) - 4
	}
	if start < len(history) {
		sb.WriteString("Recent conversation:\n")
		for _, m := range history[start:] {
			sb.WriteString(m.Role)
			sb.WriteString(": ")
			sb.WriteString(textutil.Truncate(m.Content, 200))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("New message: ")
	sb.WriteString(msgText)
	return sb.String()
}

// buildSynthesisPrompt constructs the system prompt for post-tool synthesis.
// Personality is injected first so the model adopts the right voice. Prefetch
// and temporal context ground the synthesis in the user's memories and current
// time awareness.
func buildSynthesisPrompt(dctx dispatchContext) string {
	var sb strings.Builder
	if dctx.PersonalityContent != "" {
		sb.WriteString(dctx.PersonalityContent)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Present the tool results naturally as if you already knew this information. Do not mention tools, fetching, or data sources. Write like you're talking to a friend — conversational, not robotic. Highlight what's interesting or relevant to the user.")
	if dctx.ProfileContent != "" {
		sb.WriteString("\n\n## About the user\n")
		sb.WriteString(dctx.ProfileContent)
	}
	if dctx.PrefetchContent != "" {
		sb.WriteString("\n\n## Relevant memories\n")
		sb.WriteString(dctx.PrefetchContent)
	}
	if dctx.TemporalCtx != "" {
		sb.WriteString("\n\n## Temporal context\n")
		sb.WriteString(dctx.TemporalCtx)
	}
	return sb.String()
}

// timeoutMultiStepDispatch is the overall timeout for the SubagentSupervisor
// multi-step dispatch loop.
const timeoutMultiStepDispatch = 90 * time.Second

// maxMultiStepRounds is the max number of tool-call rounds for multi-step dispatch.
const maxMultiStepRounds = 5

// tryMultiStepDispatch runs a multi-step dispatch using SubagentSupervisor.
// The subagent executes 2-3 sequential tool calls and synthesizes a response.
// Returns (handled, escalation, ack) matching tryDispatch signature.
func tryMultiStepDispatch(mc messageContext, msg *tgbotapi.Message, chatID int64,
	msgText string, dctx dispatchContext, directive string) (bool, *dispatchEscalation, string) {

	if mc.svc.Subagent == nil || mc.delegateConfig == nil {
		logger.Log.Debug("[dispatch] multi-step: missing subagent or delegateConfig, escalating")
		return false, nil, ""
	}

	logger.Log.Infof("[dispatch] multi-step: %q", textutil.Truncate(directive, 80))

	// Ack.
	sendFormatted(mc.svc.Bot, chatID, msg.MessageID, formatReply("Working on it..."))

	systemPrompt := buildMultiStepSystemPrompt(dctx)
	toolExec := tools.NewScopedToolExec(mc.registry, mc.delegateConfig)
	g := mc.delegateConfig.Grammar()

	ctx, cancel := context.WithTimeout(context.Background(), timeoutMultiStepDispatch)
	defer cancel()

	reply, err := clients.SubagentSupervisor(ctx, mc.svc.Subagent, g, systemPrompt, directive, toolExec, maxMultiStepRounds)
	if err != nil {
		logger.Log.Warnf("[dispatch] multi-step failed: %v — escalating", err)
		return false, &dispatchEscalation{Phase: "multi-step", Error: err.Error()}, ""
	}

	reply = textutil.StripThinkTags(reply)

	// Post-processing + send (shared with Brain path).
	completeMessageHandling(mc, msg, chatID, messageResult{
		Reply: reply,
		Messages: []llm.Message{
			{Role: "user", Content: msgText},
			{Role: "assistant", Content: reply},
		},
		ToolContext:       "[multi-step dispatch]\n",
		ToolsUsed:         true,
		MsgText:           msgText,
		PrefetchIDs:       dctx.PrefetchIDs,
		PrefetchSummaries: dctx.PrefetchSummaries,
	})

	logger.Log.Infof("[dispatch] multi-step handled %q (subagent path)", textutil.Truncate(msgText, 60))
	return true, nil, ""
}

// buildMultiStepSystemPrompt constructs the system prompt for the multi-step
// SubagentSupervisor loop. Includes personality, profile, memory context,
// and instructions to call tools then respond naturally.
func buildMultiStepSystemPrompt(dctx dispatchContext) string {
	var sb strings.Builder
	if dctx.PersonalityContent != "" {
		sb.WriteString(dctx.PersonalityContent)
		sb.WriteString("\n\n")
	}
	sb.WriteString(`You are a research assistant handling a multi-step request. Call the available tools as needed to gather information, then respond naturally to the user.

## Rules
- Execute the steps needed to answer the user's request.
- When you have enough information, respond with your findings.
- Be conversational and concise. Present results as if you already knew them.
- Do not mention tools, fetching, or data sources in your response.
- If a tool returns an error, try an alternative approach before giving up.`)
	if dctx.ProfileContent != "" {
		sb.WriteString("\n\n## About the user\n")
		sb.WriteString(dctx.ProfileContent)
	}
	if dctx.PrefetchContent != "" {
		sb.WriteString("\n\n## Relevant memories\n")
		sb.WriteString(dctx.PrefetchContent)
	}
	if dctx.TemporalCtx != "" {
		sb.WriteString("\n\n## Temporal context\n")
		sb.WriteString(dctx.TemporalCtx)
	}
	return sb.String()
}
