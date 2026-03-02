package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
)

type consultDeepThinkerArgs struct {
	ProblemStatement string `json:"problem_statement"`
	MaxTokens        int    `json:"max_tokens,omitempty"` // defaults to 2048 if zero
}

var deepThinkerSystemPrompt = strings.TrimSpace(prompts.DeepThinker)

// NewConsultDeepThinker returns a ToolFunc that closes over the given DeepThinkerClient
// and optional memory dependencies for context injection.
func NewConsultDeepThinker(dtc *DeepThinkerClient, pool *pgxpool.Pool, embedURL, embedModel string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		return ConsultDeepThinker(ctx, args, dtc, pool, embedURL, embedModel)
	}
}

// ConsultDeepThinker sends a problem statement to a separate deep-reasoning LLM
// and returns its response. When memory dependencies are available, it prefetches
// relevant user memories and injects them as context. On any failure it returns
// a formatted unavailability message rather than a Go error, so the MoE can
// handle it as a tool result.
func ConsultDeepThinker(ctx context.Context, args json.RawMessage, dtc *DeepThinkerClient, pool *pgxpool.Pool, embedURL, embedModel string) (string, error) {
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

	// Prefetch relevant memories to ground reasoning in the user's context.
	userContent := a.ProblemStatement
	if pool != nil && embedURL != "" {
		pf := memory.Prefetch(ctx, pool, embedURL, embedModel, a.ProblemStatement, a.ProblemStatement, 3)
		if pf != nil && pf.Content != "" {
			userContent = a.ProblemStatement + "\n\n" + pf.Content
		}
	}

	content, err := dtc.Complete(ctx, deepThinkerSystemPrompt, userContent, a.MaxTokens)
	if err != nil {
		return fmt.Sprintf("[DEEP THINKER UNAVAILABLE]: %v. Proceed with best available reasoning.", err), nil
	}

	content = textutil.StripThinkTags(content)
	return content, nil
}
