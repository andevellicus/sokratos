package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timeouts"
	"sokratos/tokens"
)

// executivePromptBase returns the Phase 2 system message template.
// Uses the embedded heartbeat_mode.txt prompt which contains a %s placeholder
// for staleness context (populated per-tick).
var executivePromptBase = strings.TrimSpace(prompts.HeartbeatMode)

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
- Check <recent_actions> to avoid repeating actions already taken.
- Do NOT continue, revisit, or follow up on previous conversations.
- Do NOT repeat information the user already knows.
- Do NOT offer unsolicited help.
- Prefer "none" unless there is a clear, actionable, time-sensitive reason to act.
- If a background task recently completed (check <recent_actions>), consider whether follow-up is needed.
- A pending task is only actionable if it is overdue or due within the next hour AND you can make progress without user input.
- If <active_goals> lists goals you can make progress on, consider initiating a plan_and_execute background task via "tool" action.
- Only pursue goals when you can make meaningful progress without user input.
- Check <work_items> to avoid re-pursuing goals already being worked on.`

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

	ctx, cancel := context.WithTimeout(context.Background(), timeouts.ObjectiveEval)
	defer cancel()

	raw, err := e.Gatekeeper.CompleteWithGrammar(ctx, prompt, contextXML, gatekeeperGrammar, tokens.GatekeeperDecision)
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

		reply, msgs, orchestratorErr := e.runOrchestrator(context.Background(), false, toolPrompt, func(opts *llm.QueryOrchestratorOpts) {
			if !conversationStale {
				opts.History = e.SM.ReadMessages()
			}
		})

		if !conversationStale {
			for _, m := range msgs {
				e.SM.AppendMessage(m)
			}
		}

		if orchestratorErr != nil {
			logger.Log.Errorf("heartbeat: orchestrator error (gatekeeper-routed): %v", orchestratorErr)
		} else if reply = strings.TrimSpace(reply); reply != "" && !strings.Contains(reply, "<NO_ACTION_REQUIRED>") {
			if e.sendDeduped(reply, "proactive response (gatekeeper-routed)") {
				e.recordAction("heartbeat", textutil.Truncate(reply, 100))
			}
		}

	case "message":
		text := strings.TrimSpace(decision.Text)
		if text == "" {
			logger.Log.Debug("heartbeat: gatekeeper returned empty message, ignoring")
			break
		}
		if e.sendDeduped(text, "gatekeeper message") {
			e.recordAction("heartbeat", textutil.Truncate(text, 100))
		}

	default:
		logger.Log.Warnf("heartbeat: gatekeeper returned unknown action %q, ignoring", decision.Action)
	}
}

// heartbeatPhase2Orchestrator runs Phase 2 via the full orchestrator (original path).
func (e *Engine) heartbeatPhase2Orchestrator(contextXML, stalenessNote string, conversationStale bool) {
	executivePrompt := fmt.Sprintf(executivePromptBase, stalenessNote)

	reply, msgs, err := e.runOrchestrator(context.Background(), false, contextXML, func(opts *llm.QueryOrchestratorOpts) {
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
		opts.History = history
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
		if e.sendDeduped(strings.TrimSpace(reply), "proactive response") {
			e.recordAction("heartbeat", textutil.Truncate(strings.TrimSpace(reply), 100))
		}
	default:
		logger.Log.Debug("heartbeat: orchestrator produced unexpected output")
	}
}
