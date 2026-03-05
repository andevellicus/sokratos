package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/tokens"
)

type consultDeepThinkerArgs struct {
	ProblemStatement string `json:"problem_statement"`
	MaxTokens        int    `json:"max_tokens,omitempty"` // defaults to 2048 if zero
}

// dtcSearchResult records one search round: the query DTC requested and the
// <retrieved_context> XML that came back.
type dtcSearchResult struct {
	query   string
	content string
}

var deepThinkerSystemPrompt = strings.TrimSpace(prompts.DeepThinker)

// dtcSearchRe matches a <SEARCH>...</SEARCH> tag emitted by DTC when it needs
// additional memory context. Case-insensitive; [\s\S] handles multi-word queries.
var dtcSearchRe = regexp.MustCompile(`(?i)<SEARCH>([\s\S]+?)</SEARCH>`)

// maxDTCSearchRounds caps how many additional memory fetches DTC may request
// per consult call (so at most maxDTCSearchRounds+1 total DTC completions).
const maxDTCSearchRounds = 2

// NewconsultDeepThinker returns a ToolFunc that closes over the given
// DeepThinkerClient and optional memory dependencies for context injection.
func NewconsultDeepThinker(dtc *clients.DeepThinkerClient, pool *pgxpool.Pool, embedURL, embedModel string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		return consultDeepThinker(ctx, args, dtc, pool, embedURL, embedModel)
	}
}

// consultDeepThinker sends a problem statement to the deep-reasoning LLM.
// It seeds the call with up to 3 prefetched memories, then runs a search loop:
// if DTC emits <SEARCH>query</SEARCH> it fetches additional memories and calls
// DTC again (up to maxDTCSearchRounds extra rounds). All retrieved memory IDs
// are tracked for usefulness scoring.
func consultDeepThinker(ctx context.Context, args json.RawMessage, dtc *clients.DeepThinkerClient, pool *pgxpool.Pool, embedURL, embedModel string) (string, error) {
	var a consultDeepThinkerArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Sprintf("[DEEP THINKER UNAVAILABLE]: invalid arguments: %v. Proceed with best available reasoning.", err), nil
	}
	if strings.TrimSpace(a.ProblemStatement) == "" {
		return "[DEEP THINKER UNAVAILABLE]: problem_statement is required. Proceed with best available reasoning.", nil
	}
	if a.MaxTokens == 0 {
		a.MaxTokens = tokens.DTCDefault
	}

	// Seed with up to 3 relevant memories before the first DTC call.
	var initialCtx string
	var allIDs []int64
	if pool != nil && embedURL != "" {
		if pf := memory.Prefetch(ctx, pool, embedURL, embedModel, a.ProblemStatement, a.ProblemStatement, 3, 0); pf != nil {
			initialCtx = pf.Content
			allIDs = pf.IDs
		}
	}

	var priorSearches []dtcSearchResult
	var lastResponse string

	for round := 0; round <= maxDTCSearchRounds; round++ {
		userContent := buildDTCContent(a.ProblemStatement, initialCtx, priorSearches)
		content, err := dtc.Complete(ctx, deepThinkerSystemPrompt, userContent, a.MaxTokens)
		if err != nil {
			if lastResponse != "" {
				// Return the last successful response rather than an error.
				break
			}
			return fmt.Sprintf("[DEEP THINKER UNAVAILABLE]: %v. Proceed with best available reasoning.", err), nil
		}
		content = textutil.StripThinkTags(content)
		lastResponse = content

		// On the final allowed round, or if DTC didn't request a search, stop.
		if round == maxDTCSearchRounds {
			break
		}
		match := dtcSearchRe.FindStringSubmatch(content)
		if match == nil {
			break
		}
		query := strings.TrimSpace(match[1])
		if query == "" || pool == nil || embedURL == "" {
			break
		}

		pf := memory.Prefetch(ctx, pool, embedURL, embedModel, query, query, 5, 0)
		if pf == nil {
			logger.Log.Debugf("[dtc] search round %d: query=%q returned no results", round+1, query)
			break
		}
		logger.Log.Debugf("[dtc] search round %d: query=%q matched %d memories", round+1, query, len(pf.IDs))
		allIDs = append(allIDs, pf.IDs...)
		priorSearches = append(priorSearches, dtcSearchResult{query: query, content: pf.Content})
	}

	if len(allIDs) > 0 && pool != nil {
		go memory.TrackRetrieval(context.Background(), pool, allIDs)
	}

	// Strip any residual <SEARCH> tags DTC left in the final answer.
	lastResponse = dtcSearchRe.ReplaceAllString(lastResponse, "")
	return strings.TrimSpace(lastResponse), nil
}

// buildDTCContent assembles the user message for a DTC call: problem statement,
// initial retrieved context, and any additional context from prior search rounds.
func buildDTCContent(problem, initialCtx string, searches []dtcSearchResult) string {
	var sb strings.Builder
	sb.WriteString(problem)
	if initialCtx != "" {
		sb.WriteString("\n\n")
		sb.WriteString(initialCtx)
	}
	for _, s := range searches {
		fmt.Fprintf(&sb, "\n\n<additional_context query=\"%s\">\n", s.query)
		sb.WriteString(s.content)
		sb.WriteString("\n</additional_context>")
	}
	return sb.String()
}
