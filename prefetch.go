package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/textutil"
	"sokratos/tools"
)

// prefetchResult holds the subconscious prefetch output: retrieved memory
// content plus the memory IDs for usefulness feedback.
type prefetchResult struct {
	IDs       []int64
	Summaries string // formatted memory summaries for system prompt injection + usefulness evaluation
}

// subconsciousPrefetch embeds the user's message and retrieves semantically
// similar memories as background context. excludePipelineID filters out
// memories from the immediately previous pipeline to prevent context bleed.
func subconsciousPrefetch(ctx context.Context, pool *pgxpool.Pool, embedURL, embedModel, msgText string, recentMessages []llm.Message, excludePipelineID int64) *prefetchResult {
	// Build trajectory string from recent user messages for contextual vector recall.
	var trajectoryParts []string
	count := 0
	for i := len(recentMessages) - 1; i >= 0 && count < 3; i-- {
		m := recentMessages[i]
		if m.Role != "user" {
			continue
		}
		text := m.Content
		if idx := strings.Index(text, "\n\n[Current Agent State]"); idx > 0 {
			text = text[:idx]
		}
		text = textutil.Truncate(text, 200)
		text = strings.TrimSpace(text)
		if text != "" {
			trajectoryParts = append(trajectoryParts, text)
			count++
		}
	}
	for i, j := 0, len(trajectoryParts)-1; i < j; i, j = i+1, j-1 {
		trajectoryParts[i], trajectoryParts[j] = trajectoryParts[j], trajectoryParts[i]
	}
	trajectoryParts = append(trajectoryParts, msgText)
	trajectoryStr := strings.Join(trajectoryParts, " | ")

	pf := memory.Prefetch(ctx, pool, embedURL, embedModel, trajectoryStr, msgText, 3, excludePipelineID)
	if pf == nil {
		return nil
	}
	if len(pf.IDs) > 0 {
		go memory.TrackRetrieval(context.Background(), pool, pf.IDs)
	}
	return &prefetchResult{
		IDs:       pf.IDs,
		Summaries: pf.Content,
	}
}

// evaluateMemoryUsefulnessViaSubagent calls the subagent to determine whether
// prefetched memories contributed to the assistant's response.
func evaluateMemoryUsefulnessViaSubagent(pool *pgxpool.Pool, sc *clients.SubagentClient, memoryIDs []int64, userMsg, assistantReply, memorySummaries string) {
	ctx, cancel := context.WithTimeout(context.Background(), tools.TimeoutUsefulnessEval)
	defer cancel()

	prompt := fmt.Sprintf("User message: %s\n\nRetrieved memories:\n%s\n\nAssistant response: %s\n\n"+
		"Were any of the retrieved memories directly useful in generating this response? "+
		"Answer with exactly YES or NO.", userMsg, memorySummaries, assistantReply)

	// GBNF grammar forces the model to output exactly "YES" or "NO" —
	// without this, Flash generates analysis text that gets truncated
	// before reaching the verdict.
	const yesNoGrammar = `root ::= "YES" | "NO"`

	content, err := sc.CompleteWithGrammar(ctx,
		"You evaluate whether specific retrieved memories were useful for generating an assistant response. "+
			"A memory is useful if its content directly informed or contributed to the response. "+
			"Topic overlap alone is not sufficient.",
		prompt, yesNoGrammar, 8)
	if err != nil {
		logger.Log.Warnf("[usefulness] subagent evaluation failed: %v", err)
		return
	}

	answer := strings.TrimSpace(strings.ToUpper(content))
	wasUseful := strings.HasPrefix(answer, "YES")
	memory.RecordMemoryUsefulness(ctx, pool, memoryIDs, wasUseful)
	logger.Log.Debugf("[usefulness] memories %v: useful=%v (raw=%q)", memoryIDs, wasUseful, answer)
}
