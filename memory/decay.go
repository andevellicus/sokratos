package memory

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
)

// MaterializeDecay reduces stored salience for memories not accessed recently.
// This ensures consolidation queries (which filter on stored salience) reflect
// actual relevance. Uses a ~30-day half-life: pow(0.977, days_since_last_access).
// Should be called periodically (e.g. on each consolidation tick).
func MaterializeDecay(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	tag, err := db.Exec(ctx,
		`UPDATE memories
		 SET salience = GREATEST(salience * pow(0.977,
		     EXTRACT(EPOCH FROM (now() - COALESCE(last_accessed, created_at))) / 86400), 0),
		     last_accessed = now()
		 WHERE last_accessed < now() - INTERVAL '1 day'
		   AND salience > 0`,
	)
	if err != nil {
		return 0, fmt.Errorf("materialize decay: %w", err)
	}

	// Regress usefulness_score toward 0.5 for memories not retrieved in 30+ days.
	// Moves 5% toward neutral each tick, preventing permanently-low scores
	// from blocking retrieval.
	_, err = db.Exec(ctx,
		`UPDATE memories
		 SET usefulness_score = usefulness_score + (0.5 - usefulness_score) * 0.05
		 WHERE COALESCE(last_retrieved_at, created_at) < now() - INTERVAL '30 days'
		   AND ABS(usefulness_score - 0.5) > 0.01`,
	)
	if err != nil {
		logger.Log.Warnf("[memory] usefulness decay failed: %v", err)
	}

	return tag.RowsAffected(), nil
}

// PruneStaleMemories deletes memories that have decayed below usefulness:
// salience <= 1, not accessed within stalenessDays, AND either superseded or
// never retrieved. Returns the number of rows deleted.
func PruneStaleMemories(ctx context.Context, db *pgxpool.Pool, stalenessDays int) (int64, error) {
	tag, err := db.Exec(ctx,
		`DELETE FROM memories
		 WHERE salience <= 1
		   AND last_accessed < now() - make_interval(days => $1)
		   AND (superseded_by IS NOT NULL OR COALESCE(retrieval_count, 0) = 0)`,
		stalenessDays,
	)
	if err != nil {
		return 0, fmt.Errorf("prune stale memories: %w", err)
	}
	return tag.RowsAffected(), nil
}
