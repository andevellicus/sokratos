package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sokratos/llm"
	"sokratos/logger"
)

// executivePromptBase is the template for the Phase 2 system message.
// It contains a %s placeholder for staleness context (populated per-tick).
const executivePromptBase = `You are running your autonomous background loop. Routines have already been executed for this tick. Review the <heartbeat_context> and determine if any proactive action is required.

%s

Priority order:
1. Pending tasks that are overdue or due within 24 hours — take action if you can make progress without user input.
2. If recent memories suggest something time-sensitive needs attention (upcoming travel, an open promise to follow up, an approaching deadline), address it.
3. Only message the user if there is something they need to know or decide RIGHT NOW. Do not message the user to report that background tasks completed silently.

ABSOLUTE RULES:
- Do NOT continue, revisit, or follow up on previous conversations. The conversation is NOT your concern here.
- Do NOT repeat information you have already told the user.
- Do NOT summarize what happened in previous conversations.
- Do NOT offer unsolicited help ("Would you like me to...", "I can also...").

CRITICAL: You MUST respond with exactly ONE of these two formats — anything else will be sent directly to the user as a Telegram message:
- If no action is needed: <NO_ACTION_REQUIRED>
- If action is needed: <TOOL_INTENT>describe the exact action to take</TOOL_INTENT>

Do NOT output status words like "idle", acknowledgements, or commentary. Your entire response must be one of the two tags above.`

// gatekeeperGrammar is a GBNF grammar constraining gatekeeper output to one of
// three JSON decision shapes: no action, tool intent, or direct message.
const gatekeeperGrammar = `root ::= none | tool | message
none ::= "{" ws "\"action\":" ws "\"none\"" ws "}"
tool ::= "{" ws "\"action\":" ws "\"tool\"" ws "," ws "\"intent\":" ws string ws "}"
message ::= "{" ws "\"action\":" ws "\"message\"" ws "," ws "\"text\":" ws string ws "}"
string ::= "\"" chars "\""
chars ::= char*
char ::= [^"\\] | "\\" escape
escape ::= ["\\nrt/]
ws ::= [ \t\n]*`

// gatekeeperPromptBase is the system prompt for the fast gatekeeper.
// It contains a %s placeholder for staleness context.
const gatekeeperPromptBase = `You are a background heartbeat gatekeeper. Your job is to evaluate the provided context and decide whether proactive action is needed.

%s

Respond with exactly ONE JSON object:
- {"action": "none"} — no action needed (the default; use this ~90%% of the time).
- {"action": "tool", "intent": "..."} — a tool-based action is needed. Describe the intent concisely.
- {"action": "message", "text": "..."} — send a short message directly to the user (only for truly urgent, time-sensitive items).

Rules:
- Do NOT continue, revisit, or follow up on previous conversations.
- Do NOT repeat information the user already knows.
- Do NOT offer unsolicited help.
- Prefer "none" unless there is a clear, actionable, time-sensitive reason to act.
- A pending task is only actionable if it is overdue or due within the next hour AND you can make progress without user input.`

// gatekeeperDecision represents the parsed gatekeeper JSON output.
type gatekeeperDecision struct {
	Action string `json:"action"`
	Intent string `json:"intent,omitempty"`
	Text   string `json:"text,omitempty"`
}

// heartbeatPhase2Gatekeeper runs Phase 2 via the fast gatekeeper (subagent/Flash).
// Only escalates to the orchestrator when the gatekeeper decides action is needed.
// Falls back to the orchestrator path on any gatekeeper error.
func (e *Engine) heartbeatPhase2Gatekeeper(contextXML, stalenessNote string, conversationStale bool) {
	prompt := fmt.Sprintf(gatekeeperPromptBase, stalenessNote)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := e.Gatekeeper.CompleteWithGrammar(ctx, prompt, contextXML, gatekeeperGrammar, 256)
	if err != nil {
		logger.Log.Warnf("heartbeat: gatekeeper error, falling back to orchestrator: %v", err)
		e.heartbeatPhase2Orchestrator(contextXML, stalenessNote, conversationStale)
		return
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		logger.Log.Info("heartbeat: gatekeeper returned empty response, treating as no action")
		return
	}

	var decision gatekeeperDecision
	if err := json.Unmarshal([]byte(raw), &decision); err != nil {
		logger.Log.Warnf("heartbeat: gatekeeper parse error, falling back to orchestrator: %v (raw: %s)", err, raw)
		e.heartbeatPhase2Orchestrator(contextXML, stalenessNote, conversationStale)
		return
	}

	switch decision.Action {
	case "none":
		logger.Log.Info("heartbeat: gatekeeper decided no action required")

	case "tool":
		logger.Log.Infof("heartbeat: gatekeeper requested tool action, routing to orchestrator: %s", decision.Intent)
		toolPrompt := fmt.Sprintf(
			"%s\n\nROUTINE: %s\nExecute this action now. Use your tools to complete it.\n"+
				"Do not message the user unless the action explicitly requires it.",
			contextXML, decision.Intent,
		)

		var reply string
		var msgs []llm.Message
		var orchestratorErr error
		e.withOrchestratorLock(func() {
			opts := e.baseOrchestratorOpts()
			if !conversationStale {
				opts.History = e.SM.ReadMessages()
			}
			reply, msgs, orchestratorErr = llm.QueryOrchestrator(
				context.Background(), e.LLM.Client, e.LLM.Model, toolPrompt,
				e.ToolExec, DefaultTrimFn, opts,
			)
		})

		if !conversationStale {
			for _, m := range msgs {
				e.SM.AppendMessage(m)
			}
		}

		if orchestratorErr != nil {
			logger.Log.Errorf("heartbeat: orchestrator error (gatekeeper-routed): %v", orchestratorErr)
		} else if reply = strings.TrimSpace(reply); reply != "" && !strings.Contains(reply, "<NO_ACTION_REQUIRED>") {
			e.sendDeduped(reply, "proactive response (gatekeeper-routed)")
		}

	case "message":
		text := strings.TrimSpace(decision.Text)
		if text == "" {
			logger.Log.Debug("heartbeat: gatekeeper returned empty message, ignoring")
			break
		}
		e.sendDeduped(text, "gatekeeper message")

	default:
		logger.Log.Warnf("heartbeat: gatekeeper returned unknown action %q, ignoring", decision.Action)
	}
}

// heartbeatPhase2Orchestrator runs Phase 2 via the full orchestrator (original path).
func (e *Engine) heartbeatPhase2Orchestrator(contextXML, stalenessNote string, conversationStale bool) {
	executivePrompt := fmt.Sprintf(executivePromptBase, stalenessNote)

	var reply string
	var msgs []llm.Message
	var err error
	e.withOrchestratorLock(func() {
		var history []llm.Message
		if !conversationStale {
			convHistory := e.SM.ReadMessages()
			history = make([]llm.Message, 0, len(convHistory)+1)
			history = append(history, convHistory...)
		} else {
			history = make([]llm.Message, 0, 1)
		}
		history = append(history, llm.Message{
			Role:    "user",
			Content: "[EXECUTIVE ROUTINE]\n" + executivePrompt,
		})

		opts := e.baseOrchestratorOpts()
		opts.History = history
		reply, msgs, err = llm.QueryOrchestrator(
			context.Background(), e.LLM.Client, e.LLM.Model, contextXML,
			e.ToolExec, DefaultTrimFn, opts,
		)
	})

	// Only persist Phase 2 messages when the conversation is active.
	if !conversationStale {
		for _, m := range msgs {
			e.SM.AppendMessage(m)
		}
	}

	switch {
	case err != nil:
		if strings.Contains(err.Error(), "too many tool call rounds") {
			logger.Log.Warn("heartbeat: max rounds reached")
			if e.SendFunc != nil {
				e.SendFunc("I started a background task but couldn't complete it. You may want to check in.")
			}
		} else {
			logger.Log.Errorf("heartbeat: orchestrator error: %v", err)
		}
	case strings.Contains(reply, "<NO_ACTION_REQUIRED>"):
		logger.Log.Info("heartbeat: no action required")
	case strings.TrimSpace(reply) != "":
		e.sendDeduped(strings.TrimSpace(reply), "proactive response")
	default:
		logger.Log.Debug("heartbeat: orchestrator produced unexpected output")
	}
}
