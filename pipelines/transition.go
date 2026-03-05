package pipelines

import (
	"context"
	"fmt"
	"strings"

	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/tokens"
)

// generateTransitionMemory is the synchronous core that creates a transition
// memory re-contextualizing older memories in light of a paradigm-shifting event.
// Returns the new memory ID (0 if skipped) and any error.
func generateTransitionMemory(
	ctx context.Context,
	deps PipelineDeps,
	newEventSummary string,
	tags []string,
) (int64, error) {
	if deps.DTC == nil {
		return 0, nil
	}

	// Embed the new event for similarity search.
	emb, err := memory.GetEmbedding(ctx, deps.EmbedEndpoint, deps.EmbedModel, newEventSummary)
	if err != nil {
		return 0, fmt.Errorf("embedding failed: %w", err)
	}

	// Query top 10 related non-superseded memories from last 6 months.
	rows, err := deps.Pool.Query(ctx,
		`SELECT id, summary FROM memories
		 WHERE superseded_by IS NULL
		   AND (embedding <=> $1) < 0.4
		   AND created_at >= now() - INTERVAL '6 months'
		 ORDER BY (embedding <=> $1)
		 LIMIT 10`,
		pgvector.NewVector(emb),
	)
	if err != nil {
		return 0, fmt.Errorf("related memory query failed: %w", err)
	}
	defer rows.Close()

	type relatedMem struct {
		ID      int64
		Summary string
	}
	var related []relatedMem
	for rows.Next() {
		var m relatedMem
		if err := rows.Scan(&m.ID, &m.Summary); err != nil {
			continue
		}
		related = append(related, m)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("row iteration error: %w", err)
	}

	if len(related) < 2 {
		logger.Log.Debugf("[transition] skipping — only %d related memories found", len(related))
		return 0, nil
	}

	// Build prompt with new event + related memories.
	var b strings.Builder
	fmt.Fprintf(&b, "NEW EVENT:\n%s\n\nRELATED MEMORIES:\n", newEventSummary)
	for i, m := range related {
		summary := textutil.Truncate(m.Summary, 300)
		fmt.Fprintf(&b, "%d. %s\n", i+1, summary)
	}

	// Call deep thinker with thinking enabled for creative synthesis.
	raw, err := deps.DTC.Complete(ctx, strings.TrimSpace(prompts.Transition), b.String(), tokens.DTCTransition)
	if err != nil {
		return 0, fmt.Errorf("deep thinker synthesis failed: %w", err)
	}

	raw = textutil.StripThinkTags(raw)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		logger.Log.Warn("[transition] deep thinker returned empty output")
		return 0, nil
	}

	// Save transition memory with high salience, skip quality scoring.
	transitionTags := append([]string{"transition"}, tags...)
	req := memory.MemoryWriteRequest{
		Summary:       raw,
		Tags:          transitionTags,
		Salience:      10,
		MemoryType:    "transition",
		Source:        "transition",
		EmbedEndpoint: deps.EmbedEndpoint,
		EmbedModel:    deps.EmbedModel,
	}
	id, err := memory.ScoreAndWrite(ctx, deps.Pool, req, nil, nil) // nil grammarFn/queueFn = skip enrichment
	if err != nil {
		return 0, fmt.Errorf("save failed: %w", err)
	}

	logger.Log.Infof("[transition] saved transition memory id=%d for event: %s", id, newEventSummary)
	return id, nil
}

