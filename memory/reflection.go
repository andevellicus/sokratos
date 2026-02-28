package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/textutil"
)

// CountMemoriesSince returns the number of substantive memories created after
// the given timestamp. When since is zero, counts all non-superseded memories.
// Used by the cognitive processing trigger to count memories since last run.
func CountMemoriesSince(ctx context.Context, db *pgxpool.Pool, since time.Time) (int, error) {
	var count int
	var cutoff time.Time
	if since.IsZero() {
		cutoff = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		cutoff = since
	}
	err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM memories
		 WHERE superseded_by IS NULL
		   AND memory_type NOT IN ('reflection', 'episode', 'identity')
		   AND created_at > $1`,
		cutoff,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count memories since %v: %w", cutoff, err)
	}
	return count, nil
}

// CountMemoriesSinceLastReflection returns the number of substantive memories
// created since the last reflection. This is used for trigger-based reflection.
func CountMemoriesSinceLastReflection(ctx context.Context, db *pgxpool.Pool) (int, error) {
	var count int
	err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM memories
		 WHERE superseded_by IS NULL
		   AND memory_type NOT IN ('reflection', 'episode', 'identity')
		   AND created_at > COALESCE(
		       (SELECT MAX(created_at) FROM memories WHERE memory_type = 'reflection'),
		       '1970-01-01'::timestamptz
		   )`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count memories since last reflection: %w", err)
	}
	return count, nil
}

// TrackRetrieval increments retrieval_count, resets last_retrieved_at and
// last_accessed, and applies a dampened salience bump for the given memory IDs.
// Dampened curve: salience + 0.3 * (1 - salience/10) pushes toward 10 with
// diminishing returns at high salience values.
// Synchronous — callers decide whether to run in a goroutine.
func TrackRetrieval(ctx context.Context, db *pgxpool.Pool, ids []int64) {
	if len(ids) == 0 {
		return
	}
	_, err := db.Exec(ctx,
		`UPDATE memories
		 SET retrieval_count = COALESCE(retrieval_count, 0) + 1,
		     last_retrieved_at = NOW(),
		     last_accessed = NOW(),
		     salience = LEAST(COALESCE(salience, 5) + (0.3 * (1.0 - COALESCE(salience, 5) / 10.0)), 10)
		 WHERE id = ANY($1)`,
		ids,
	)
	if err != nil {
		logger.Log.Warnf("[memory] failed to track retrieval: %v", err)
	}
}

// --- Reflection / Meta-Cognition ---

// ReflectOnMemories performs a meta-cognitive reflection over memories created
// since the given time, identifying patterns, evolving interests, connections,
// and predictions. Returns the reflection memory ID, or 0 if skipped.
func ReflectOnMemories(ctx context.Context, db *pgxpool.Pool, embedEndpoint, embedModel, reflectionPrompt string, synthesize SynthesizeFunc, grammarFn GrammarSubagentFunc, since time.Time) (int64, error) {
	// Query memories since the given time (excluding reflections), grouped by source/type.
	rows, err := db.Query(ctx,
		`SELECT source, memory_type, summary
		 FROM memories
		 WHERE superseded_by IS NULL
		   AND memory_type != 'reflection'
		   AND created_at >= $1
		 ORDER BY source, memory_type, created_at DESC
		 LIMIT 100`,
		since,
	)
	if err != nil {
		return 0, fmt.Errorf("query memories for reflection: %w", err)
	}
	defer rows.Close()

	// Group summaries under source/type headers, cap 10 per group.
	type groupKey struct{ Source, MemoryType string }
	groups := make(map[groupKey][]string)
	var orderedKeys []groupKey
	totalCount := 0

	for rows.Next() {
		var source, memType, summary string
		if err := rows.Scan(&source, &memType, &summary); err != nil {
			continue
		}
		key := groupKey{source, memType}
		if len(groups[key]) == 0 {
			orderedKeys = append(orderedKeys, key)
		}
		if len(groups[key]) < 10 {
			summary = textutil.Truncate(summary, 300)
			groups[key] = append(groups[key], summary)
			totalCount++
		}
	}

	if totalCount < 5 {
		return 0, nil // Not enough data for meaningful reflection.
	}

	// Build structured input.
	var sb strings.Builder
	for _, key := range orderedKeys {
		fmt.Fprintf(&sb, "=== %s/%s ===\n", key.Source, key.MemoryType)
		for _, s := range groups[key] {
			fmt.Fprintf(&sb, "- %s\n", s)
		}
		sb.WriteByte('\n')
	}

	// Call synthesize with reflection prompt.
	raw, err := synthesize(ctx, reflectionPrompt, sb.String())
	if err != nil {
		return 0, fmt.Errorf("reflection synthesis failed: %w", err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("reflection produced empty output")
	}

	// Embed and insert the reflection.
	embedded, err := embedWithFallback(ctx, embedEndpoint, embedModel, raw)
	if err != nil {
		return 0, fmt.Errorf("reflection embedding failed: %w", err)
	}

	var id int64
	for _, ec := range embedded {
		var chunkID int64
		err = db.QueryRow(ctx,
			`INSERT INTO memories (summary, embedding, salience, tags, memory_type, source, entities)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING id`,
			ec.Text, pgvector.NewVector(ec.Embedding), 9.0, []string{"reflection", "meta"},
			"reflection", "reflection", []string{},
		).Scan(&chunkID)
		if err != nil {
			return 0, fmt.Errorf("reflection insert failed: %w", err)
		}
		if id == 0 {
			id = chunkID
		}
	}

	logger.Log.Infof("[memory] reflection saved id=%d (%d bytes from %d memories)", id, len(raw), totalCount)

	// Fire async entity enrichment for the reflection.
	if grammarFn != nil && id > 0 {
		go EnrichViaGrammarFn(db, grammarFn, []int64{id}, raw, 9.0, nil)
	}

	return id, nil
}
