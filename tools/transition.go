package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
)

// GenerateTransitionMemoryAsync creates a transition memory that
// re-contextualizes older memories in light of a paradigm-shifting event.
// Runs as a fire-and-forget goroutine.
func GenerateTransitionMemoryAsync(
	pool *pgxpool.Pool,
	embedEndpoint, embedModel string,
	dtc *DeepThinkerClient,
	newEventSummary string,
	tags []string,
) {
	go func() {
		if dtc == nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), TimeoutTransition)
		defer cancel()

		// Embed the new event for similarity search.
		emb, err := memory.GetEmbedding(ctx, embedEndpoint, embedModel, newEventSummary)
		if err != nil {
			logger.Log.Warnf("[transition] embedding failed: %v", err)
			return
		}

		// Query top 10 related non-superseded memories from last 6 months.
		rows, err := pool.Query(ctx,
			`SELECT id, summary FROM memories
			 WHERE superseded_by IS NULL
			   AND (embedding <=> $1) < 0.4
			   AND created_at >= now() - INTERVAL '6 months'
			 ORDER BY (embedding <=> $1)
			 LIMIT 10`,
			pgvector.NewVector(emb),
		)
		if err != nil {
			logger.Log.Warnf("[transition] related memory query failed: %v", err)
			return
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
			logger.Log.Warnf("[transition] row iteration error: %v", err)
			return
		}

		if len(related) < 2 {
			logger.Log.Debugf("[transition] skipping — only %d related memories found", len(related))
			return
		}

		// Build prompt with new event + related memories.
		var b strings.Builder
		fmt.Fprintf(&b, "NEW EVENT:\n%s\n\nRELATED MEMORIES:\n", newEventSummary)
		for i, m := range related {
			summary := textutil.Truncate(m.Summary, 300)
			fmt.Fprintf(&b, "%d. %s\n", i+1, summary)
		}

		// Call deep thinker with thinking enabled for creative synthesis.
		raw, err := dtc.Complete(ctx, strings.TrimSpace(prompts.Transition), b.String(), 1024)
		if err != nil {
			logger.Log.Warnf("[transition] deep thinker synthesis failed: %v", err)
			return
		}

		raw = textutil.StripThinkTags(raw)
		raw = strings.TrimSpace(raw)
		if raw == "" {
			logger.Log.Warn("[transition] deep thinker returned empty output")
			return
		}

		// Save transition memory with high salience, skip quality scoring.
		transitionTags := append([]string{"transition"}, tags...)
		req := memory.MemoryWriteRequest{
			Summary:       raw,
			Tags:          transitionTags,
			Salience:      10,
			MemoryType:    "transition",
			Source:        "transition",
			EmbedEndpoint: embedEndpoint,
			EmbedModel:    embedModel,
		}
		id, err := memory.ScoreAndWrite(ctx, pool, req, nil) // nil grammarFn = skip quality scoring
		if err != nil {
			logger.Log.Warnf("[transition] save failed: %v", err)
			return
		}

		logger.Log.Infof("[transition] saved transition memory id=%d for event: %s", id, newEventSummary)
	}()
}
