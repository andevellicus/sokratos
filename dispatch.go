package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"sokratos/clients"
	"sokratos/engine"
	"sokratos/grammar"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timefmt"
	"sokratos/tools"
)

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

// dispatchResult is the parsed output from the triage grammar.
type dispatchResult struct {
	Dispatch  bool            `json:"dispatch"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Multi     bool            `json:"multi"`
	Directive string          `json:"directive"`
	Ack       string          `json:"ack"`
}

// dispatchEscalation captures context from a failed dispatch attempt so
// the Brain can avoid repeating the same failing call.
type dispatchEscalation struct {
	ToolName   string // empty if triage itself failed
	Error      string // error description
	Phase      string // "triage" | "execution" | "synthesis" | "multi-step"
	ToolResult string // truncated successful tool result (non-empty when synthesis failed after tool succeeded)
}

// neverDispatchTools is the set of tools that must always be escalated to the
// Brain, even if the triage model tries to dispatch them. Defense-in-depth
// complement to the prompt-level rule.
var neverDispatchTools = map[string]bool{
	"send_email":         true,
	"create_event":       true,
	"create_skill":       true,
	"manage_skills":      true,
	"manage_routines":    true,
	"manage_personality": true,
	"save_memory":        true,
	"forget_topic":       true,
	"reason":             true,
	"plan_and_execute":   true,
	"delegate_task":      true,
	"ask_database":       true,
	"manage_objectives":  true,
	"write_file":         true,
	"patch_file":         true,
	"update_skill":       true,
	"reply_to_job":       true,
	"cancel_job":         true,
}

// brainSessionPrompts maps task types to their Brain session system prompts.
var brainSessionPrompts = map[string]string{
	"create_skill": prompts.SessionCreateSkill,
	"send_email":   prompts.SessionSendEmail,
}

// brainSessionAcks maps tool names to acknowledgement messages sent when
// a background Brain job is spawned.
var brainSessionAcks = map[string]string{
	"create_skill": "I'll work on creating that skill in the background. You can keep chatting — I'll let you know when it's ready or if I have questions.",
	"send_email":   "I'll draft that email in the background. You can keep chatting — I'll send it your way for review shortly.",
}

// mandatedBrainTools maps tools that MUST run as background Brain jobs to their
// task_type (used for session prompt selection). The 9B is intercepted at the
// supervisor level if it tries to call these directly.
var mandatedBrainTools = map[string]string{
	"create_skill": "create_skill",
	"update_skill": "create_skill", // same session prompt as create_skill
}

// ---------------------------------------------------------------------------
// Background Brain jobs — async sessions for complex tool calls.
// ---------------------------------------------------------------------------

// buildJobContext generates a system prompt injection block describing active
// background jobs so the 9B can route user messages to them.
func buildJobContext(jobs []*engine.BackgroundJob) string {
	if len(jobs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Active Background Jobs]\n")
	sb.WriteString("Background Brain jobs are running. If the user's message relates to one, call reply_to_job or cancel_job.\n\n")
	for _, j := range jobs {
		isActive, lastQ, _ := j.Snapshot()
		status := "working"
		if !isActive && lastQ != "" {
			status = "waiting for input"
		}
		fmt.Fprintf(&sb, "- Job %s: tool=%s, goal=%q, status=%s", j.ID, j.Tool, textutil.Truncate(j.UserGoal, 100), status)
		if lastQ != "" {
			fmt.Fprintf(&sb, ", last_question=%q", textutil.Truncate(lastQ, 150))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// runBackgroundJob runs a Brain session in a goroutine, executing multi-round
// tool calls with the ability to ask the user questions and receive replies.
func runBackgroundJob(mc messageContext, job *engine.BackgroundJob) {
	defer mc.svc.StateMgr.RemoveJob(job.ID)

	// Select session prompt by TaskType, falling back to general reasoning prompt.
	sessionPrompt := brainSessionPrompts[job.TaskType]
	if sessionPrompt == "" {
		sessionPrompt = prompts.SessionReason
	}

	messages := []llm.Message{
		{Role: "system", Content: sessionPrompt},
		{Role: "user", Content: job.UserGoal},
	}

	const maxRounds = 20

	for range maxRounds {
		ctx, cancel := context.WithCancel(context.Background())
		job.SetActive(true, cancel)

		choice := mc.router.AcquireOrFallback(ctx, true) // preferBrain

		// Wrap toolExec to detect when the triggering tool succeeds.
		sessionToolExec := func(execCtx context.Context, raw json.RawMessage) (string, error) {
			var call struct {
				Name string `json:"name"`
			}
			json.Unmarshal(raw, &call)
			result, err := mc.confirmExec(execCtx, raw)
			if err == nil && call.Name == job.Tool && !llm.IsToolSoftError(result) {
				job.SetToolSucceeded(true)
			}
			return result, err
		}

		reply, newMsgs, qErr := llm.QueryOrchestrator(ctx, choice.Client, choice.Model,
			messages[len(messages)-1].Content, sessionToolExec, nil, &llm.QueryOrchestratorOpts{
				ToolAgent: mc.lb.ToolAgent,
				History:   messages[:len(messages)-1],
				// No MandatedBrainTools — Brain should execute tools directly.
			})

		choice.Release()
		cancel()
		job.SetActive(false, nil)

		if qErr != nil {
			if ctx.Err() != nil {
				sendFormatted(mc.svc.Bot, job.ChatID, 0, formatReply("Background job cancelled."))
			} else {
				logger.Log.Warnf("[job:%s] error: %v", job.ID, qErr)
				sendFormatted(mc.svc.Bot, job.ChatID, 0, formatReply("Background job error: "+qErr.Error()))
			}
			return
		}

		messages = append(messages, newMsgs...)
		_, _, toolSucceeded := job.Snapshot()

		if toolSucceeded {
			// Tool succeeded — send the Brain's final reply and record in conversation.
			sendFormatted(mc.svc.Bot, job.ChatID, 0, formatReply(reply))
			mc.svc.StateMgr.AppendMessage(llm.Message{
				Role:    "assistant",
				Content: fmt.Sprintf("[Background %s completed: %s]", job.Tool, textutil.Truncate(reply, 200)),
			})
			return
		}

		// Brain has a question or produced output — send to user, park goroutine.
		sendFormatted(mc.svc.Bot, job.ChatID, 0, formatReply(reply))
		job.SetLastQuestion(reply)

		// Park — slot released, goroutine blocks waiting for user input.
		input, inputOK := <-job.InputCh
		if !inputOK {
			sendFormatted(mc.svc.Bot, job.ChatID, 0, formatReply("Background job cancelled."))
			return
		}
		job.SetLastQuestion("")
		messages = append(messages, llm.Message{Role: "user", Content: input})
	}

	sendFormatted(mc.svc.Bot, job.ChatID, 0, formatReply("Background job reached maximum rounds without completing."))
}

// ---------------------------------------------------------------------------
// Subagent dispatch: lightweight triage that routes simple tool calls around
// the Brain entirely.
// ---------------------------------------------------------------------------

// dispatchProgressInterval is how often to send a "still working..." update
// to the user during long-running tool execution in the dispatch path.
const dispatchProgressInterval = 20 * time.Second

// tryDispatch attempts to handle a user message via the subagent dispatch path
// (triage → tool call → synthesis) without involving the Brain. Returns
// (handled, escalation, ack). handled=true means fully handled. ack is the
// triage-generated acknowledgement text for escalations (dispatch:false).
func tryDispatch(mc messageContext, msg *tgbotapi.Message, chatID int64,
	msgText string, dctx dispatchContext, history []llm.Message) (bool, *dispatchEscalation, string) {

	if mc.svc.Subagent == nil {
		return false, nil, ""
	}

	// --- Triage ---
	used, total := mc.svc.Subagent.SlotsInUse()
	logger.Log.Debugf("[dispatch] triage starting (subagent slots: %d/%d used)", used, total)

	triagePrompt := buildTriageSystemPrompt(mc.registry, timefmt.FormatNatural(time.Now()))
	triageInput := buildTriageInput(msgText, history)

	triageCtx, triageCancel := context.WithTimeout(context.Background(), tools.TimeoutDispatchTriage)
	raw, err := mc.svc.Subagent.TryCompleteWithGrammarThinking(triageCtx, triagePrompt, triageInput, grammar.BuildDispatchGrammar(), dispatchMaxTriageTokens)
	triageCancel()
	if err != nil {
		logger.Log.Debugf("[dispatch] triage skipped (subagent slots: %d/%d): %v", used, total, err)
		return false, nil, "One moment..." // slots busy — clean escalation
	}

	var dr dispatchResult
	if err := json.Unmarshal([]byte(raw), &dr); err != nil {
		logger.Log.Warnf("[dispatch] triage parse failed: %v — raw: %s", err, textutil.Truncate(raw, 200))
		return false, nil, "One moment..."
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
		return false, nil, dr.Ack
	}
	if neverDispatchTools[dr.Tool] {
		logger.Log.Warnf("[dispatch] triage tried to dispatch never-dispatch tool %q, forcing escalation", dr.Tool)
		return false, nil, dr.Ack
	}

	used, total = mc.svc.Subagent.SlotsInUse()
	logger.Log.Infof("[dispatch] dispatching %s (subagent slots: %d/%d used)", dr.Tool, used, total)

	// Send LLM-generated ack if the triage model wrote one.
	if dr.Ack != "" {
		ackFm := formatReply(dr.Ack)
		if _, ackErr := sendFormatted(mc.svc.Bot, chatID, msg.MessageID, ackFm); ackErr != nil {
			logger.Log.Warnf("[dispatch] ack send failed: %v", ackErr)
		} else {
			logger.Log.Debugf("[dispatch] ack sent for %s: %q", dr.Tool, textutil.Truncate(dr.Ack, 60))
		}
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

	toolCtx, toolCancel := context.WithTimeout(context.Background(), tools.TimeoutDispatchToolExec)
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
	synthesisPrompt := buildContextualPrompt(dctx, "Present the tool results naturally as if you already knew this information. Do not mention tools, fetching, or data sources. Write like you're talking to a friend — conversational, not robotic. Highlight what's interesting or relevant to the user.")
	truncatedResult := result
	if len(truncatedResult) > dispatchMaxResultLen {
		truncatedResult = truncatedResult[:dispatchMaxResultLen] + "\n... (truncated)"
	}
	synthesisInput := fmt.Sprintf("The user said: %s\n\nHere's what came back:\n%s", msgText, truncatedResult)

	synthCtx, synthCancel := context.WithTimeout(context.Background(), tools.TimeoutDispatchSynthesis)
	reply, synthErr := mc.svc.Subagent.Complete(synthCtx, synthesisPrompt, synthesisInput, dispatchMaxSynthTokens)
	synthCancel()

	if synthErr != nil {
		// Tier 2: Try DTC CompleteNoThink as lightweight synthesis fallback.
		if mc.svc.DTC != nil {
			logger.Log.Infof("[dispatch] subagent synthesis failed, trying DTC fallback for %s", dr.Tool)
			dtcCtx, dtcCancel := context.WithTimeout(context.Background(), tools.TimeoutDispatchDTCSynthesis)
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
		PipelineID:        int64(msg.MessageID),
	})

	totalElapsed := time.Since(toolStart)
	logger.Log.Infof("[dispatch] handled %q via %s in %s (subagent path)", textutil.Truncate(msgText, 60), dr.Tool, totalElapsed.Round(time.Millisecond))
	return true, nil, ""
}

// buildTriageSystemPrompt constructs the system prompt for dispatch triage.
func buildTriageSystemPrompt(registry *tools.Registry, currentTime string) string {
	toolIndex := registry.CompactIndex()
	if skills := registry.DynamicSkillDescriptions(); skills != "" {
		toolIndex += "\n" + skills
	}

	prompt := strings.Replace(prompts.DispatchTriage, "%CURRENT_TIME%", currentTime, 1)
	prompt = strings.Replace(prompt, "%TOOL_INDEX%", toolIndex, 1)
	return prompt
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

// buildContextualPrompt constructs a system prompt from a dispatchContext by
// prepending personality, appending the instructions block, then adding
// profile, prefetch, and temporal context sections.
func buildContextualPrompt(dctx dispatchContext, instructions string) string {
	var sb strings.Builder
	if dctx.PersonalityContent != "" {
		sb.WriteString(dctx.PersonalityContent)
		sb.WriteString("\n\n")
	}
	sb.WriteString(instructions)
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

	systemPrompt := buildContextualPrompt(dctx, `You are a research assistant handling a multi-step request. Call the available tools as needed to gather information, then respond naturally to the user.

## Rules
- Execute the steps needed to answer the user's request.
- When you have enough information, respond with your findings.
- Be conversational and concise. Present results as if you already knew them.
- Do not mention tools, fetching, or data sources in your response.
- If a tool returns an error, try an alternative approach before giving up.`)
	toolExec := tools.NewScopedToolExec(mc.registry, mc.delegateConfig)
	g := mc.delegateConfig.Grammar()

	ctx, cancel := context.WithTimeout(context.Background(), tools.TimeoutMultiStepDispatch)
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
		PipelineID:        int64(msg.MessageID),
	})

	logger.Log.Infof("[dispatch] multi-step handled %q (subagent path)", textutil.Truncate(msgText, 60))
	return true, nil, ""
}

// Dispatch token/result limits.
const (
	dispatchMaxTriageTokens = 768
	dispatchMaxSynthTokens  = 2048
	dispatchMaxResultLen    = 8000
	maxMultiStepRounds      = 5
)
