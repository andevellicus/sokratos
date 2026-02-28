package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/logger"
)

// SubagentToolExec executes a tool call given its raw JSON ({"name":"...","arguments":{...}}).
// Returns the tool result string or an error.
type SubagentToolExec func(ctx context.Context, raw json.RawMessage) (string, error)

// SubagentSupervisor runs a lightweight multi-turn tool execution loop for a
// subagent. The subagent is grammar-constrained to produce either a tool call
// or a final response. Tool results are injected back as user messages.
//
// The loop terminates when the subagent produces a "respond" action or
// maxRounds is exceeded.
func SubagentSupervisor(ctx context.Context, sc *SubagentClient, grammar string,
	systemPrompt string, directive string, toolExec SubagentToolExec, maxRounds int) (string, error) {

	messages := []dtcMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: directive},
	}

	for round := 0; round < maxRounds; round++ {
		raw, err := sc.CompleteMultiTurnWithGrammar(ctx, messages, grammar, 2048)
		if err != nil {
			return "", fmt.Errorf("subagent round %d: %w", round, err)
		}

		var decision struct {
			Action    string          `json:"action"`
			Name      string          `json:"name,omitempty"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
			Text      string          `json:"text,omitempty"`
		}
		if err := json.Unmarshal([]byte(raw), &decision); err != nil {
			return "", fmt.Errorf("parse subagent decision round %d: %w", round, err)
		}

		if decision.Action == "respond" {
			logger.Log.Infof("[subagent-supervisor] completed in %d round(s)", round+1)
			return decision.Text, nil
		}

		if decision.Action != "tool" {
			return "", fmt.Errorf("subagent returned unknown action %q", decision.Action)
		}

		// Build a tool call JSON and execute it.
		toolJSON, _ := json.Marshal(map[string]any{"name": decision.Name, "arguments": decision.Arguments})
		logger.Log.Infof("[subagent-supervisor] round %d: calling %s", round+1, decision.Name)
		result, execErr := toolExec(ctx, toolJSON)
		if execErr != nil {
			result = fmt.Sprintf("Tool error: %v", execErr)
		}

		messages = append(messages,
			dtcMessage{Role: "assistant", Content: raw},
			dtcMessage{Role: "user", Content: "Tool result: " + result},
		)
	}
	return "", fmt.Errorf("subagent exceeded %d rounds", maxRounds)
}
