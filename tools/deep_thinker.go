package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/prompts"
	"sokratos/textutil"
)

type consultDeepThinkerArgs struct {
	ProblemStatement string `json:"problem_statement"`
	MaxTokens        int    `json:"max_tokens,omitempty"` // defaults to 2048 if zero
}

var deepThinkerSystemPrompt = strings.TrimSpace(prompts.DeepThinker)

// ConsultDeepThinker sends a problem statement to a separate deep-reasoning LLM
// and returns its response. On any failure it returns a formatted unavailability
// message rather than a Go error, so the MoE can handle it as a tool result.
// VRAM access tracking is handled centrally by DeepThinkerClient.OnAccess.
func ConsultDeepThinker(ctx context.Context, args json.RawMessage, dtc *DeepThinkerClient) (string, error) {
	var a consultDeepThinkerArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Sprintf("[DEEP THINKER UNAVAILABLE]: invalid arguments: %v. Proceed with best available reasoning.", err), nil
	}
	if strings.TrimSpace(a.ProblemStatement) == "" {
		return "[DEEP THINKER UNAVAILABLE]: problem_statement is required. Proceed with best available reasoning.", nil
	}
	if a.MaxTokens == 0 {
		a.MaxTokens = 2048
	}

	content, err := dtc.Complete(ctx, deepThinkerSystemPrompt, a.ProblemStatement, a.MaxTokens)
	if err != nil {
		return fmt.Sprintf("[DEEP THINKER UNAVAILABLE]: %v. Proceed with best available reasoning.", err), nil
	}

	content = textutil.StripThinkTags(content)
	return content, nil
}
