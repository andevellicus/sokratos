package clients

import (
	"context"
	"encoding/json"

	"sokratos/orchestrate"
	"sokratos/tokens"
)

// SubagentToolExec executes a tool call given its raw JSON ({"name":"...","arguments":{...}}).
// Returns the tool result string or an error.
type SubagentToolExec func(ctx context.Context, raw json.RawMessage) (string, error)

// SubagentSupervisor runs a multi-turn tool execution loop for a subagent,
// delegating to the unified orchestrate.RunLoop. The subagent is grammar-
// constrained to produce either a tool call or a final response.
//
// progressFn, if non-nil, is called at the start of each tool round with a
// human-readable status (e.g. "Step 2/5: calling search_web").
func SubagentSupervisor(ctx context.Context, sc *SubagentClient, grammar string,
	systemPrompt string, directive string, toolExec SubagentToolExec, maxRounds int,
	progressFn func(string)) (string, error) {

	// Wrap SubagentClient.CompleteMultiTurnWithGrammar as orchestrate.ChatFunc.
	chatFn := func(ctx context.Context, req orchestrate.ChatInput) (string, error) {
		msgs := make([]chatMessage, len(req.Messages))
		for i, m := range req.Messages {
			msgs[i] = chatMessage{Role: m.Role, Content: m.Content}
		}
		return sc.CompleteMultiTurnWithGrammar(ctx, msgs, req.Grammar, req.MaxTokens)
	}

	messages := []orchestrate.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: directive},
	}

	cfg := orchestrate.LoopConfig{
		Grammar:            grammar,
		MaxTokens:          tokens.SubagentSupervisor,
		MaxRounds:          maxRounds,
		ProgressFn:         progressFn,
		SoftErrorDetection: true,
	}

	response, _, err := orchestrate.RunLoop(ctx, chatFn, messages, orchestrate.ToolExecFunc(toolExec), cfg)
	return response, err
}
