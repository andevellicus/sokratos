package clients

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/logger"
	"sokratos/textutil"
)

const defaultSubagentToolResultLen = 8000

// SubagentToolExec executes a tool call given its raw JSON ({"name":"...","arguments":{...}}).
// Returns the tool result string or an error.
type SubagentToolExec func(ctx context.Context, raw json.RawMessage) (string, error)

// maxSubagentErrorRetries is the number of free tool-error retries that don't
// count against the round budget.
const maxSubagentErrorRetries = 3

// SubagentSupervisor runs a lightweight multi-turn tool execution loop for a
// subagent. The subagent is grammar-constrained to produce either a tool call
// or a final response. Tool results are injected back as user messages.
//
// Tool errors don't consume a round (up to maxSubagentErrorRetries free retries).
// The loop terminates when the subagent produces a "respond" action or
// usedRounds reaches maxRounds.
func SubagentSupervisor(ctx context.Context, sc *SubagentClient, grammar string,
	systemPrompt string, directive string, toolExec SubagentToolExec, maxRounds int) (string, error) {

	messages := []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: directive},
	}

	usedRounds := 0
	errorRetries := 0

	for usedRounds < maxRounds {
		raw, err := sc.CompleteMultiTurnWithGrammar(ctx, messages, grammar, 2048)
		if err != nil {
			return "", fmt.Errorf("subagent round %d: %w", usedRounds, err)
		}

		var decision struct {
			Action    string          `json:"action"`
			Name      string          `json:"name,omitempty"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
			Text      string          `json:"text,omitempty"`
		}
		if err := json.Unmarshal([]byte(raw), &decision); err != nil {
			// Try cleaning LLM artifacts (code fences, trailing commas, etc.)
			cleaned := textutil.CleanLLMJSON(raw)
			if err2 := json.Unmarshal([]byte(cleaned), &decision); err2 != nil {
				return "", fmt.Errorf("parse subagent decision round %d: %w (raw: %.200s)", usedRounds, err, raw)
			}
		}

		if decision.Action == "respond" {
			logger.Log.Infof("[subagent-supervisor] completed in %d round(s)", usedRounds+1)
			return decision.Text, nil
		}

		if decision.Action != "tool" {
			return "", fmt.Errorf("subagent returned unknown action %q", decision.Action)
		}

		// Build a tool call JSON and execute it.
		toolJSON, _ := json.Marshal(map[string]any{"name": decision.Name, "arguments": decision.Arguments})
		logger.Log.Infof("[subagent-supervisor] round %d: calling %s", usedRounds+1, decision.Name)
		result, execErr := toolExec(ctx, toolJSON)

		var toolResultMsg string
		if execErr != nil {
			errorRetries++
			if errorRetries <= maxSubagentErrorRetries {
				toolResultMsg = fmt.Sprintf("Tool error (attempt %d/%d): %v\nReformulate with corrected parameters or try a different tool.",
					errorRetries, maxSubagentErrorRetries, execErr)
				// Don't increment usedRounds — free retry.
			} else {
				toolResultMsg = fmt.Sprintf("Tool error (retries exhausted): %v", execErr)
				usedRounds++
			}
		} else {
			errorRetries = 0
			usedRounds++
			result = textutil.TruncateToolResult(result, defaultSubagentToolResultLen, "Use specific queries or filters to narrow results")
			toolResultMsg = result
		}

		messages = append(messages,
			chatMessage{Role: "assistant", Content: raw},
			chatMessage{Role: "user", Content: "Tool result: " + toolResultMsg},
		)
	}
	return "", fmt.Errorf("subagent exceeded %d rounds", maxRounds)
}
