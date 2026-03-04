package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Salience thresholds used across the codebase.
const (
	SalienceHigh   = 8.0 // consolidation
	SalienceMedium = 6.0 // temporal context, briefing
	SalienceLow    = 5.0 // inference, curiosity signal
)

// Type exclusion sets for memory queries.
var (
	// ExcludeSynthetic excludes all synthetic memory types: goals, curiosity, briefing queries.
	ExcludeSynthetic = []string{"identity", "reflection", "episode"}

	// ExcludeInternal excludes identity and reflection: prefetch, triage, search.
	ExcludeInternal = []string{"identity", "reflection"}

	// ExcludeEpisodic excludes episode and reflection: episode synthesis queries.
	ExcludeEpisodic = []string{"episode", "reflection"}
)

// FormatSQLExclusion formats a type exclusion set for use in SQL IN clauses.
// Returns a string like "'identity', 'reflection', 'episode'" for direct
// interpolation into queries. Only use with package-level constant slices.
func FormatSQLExclusion(types []string) string {
	quoted := make([]string, len(types))
	for i, t := range types {
		quoted[i] = "'" + t + "'"
	}
	return strings.Join(quoted, ", ")
}

// CountRecentMemories counts non-backfill memories with salience >= minSalience
// created within the last `hours` hours, excluding synthetic types.
func CountRecentMemories(ctx context.Context, db *pgxpool.Pool, hours int, minSalience float64) (int, error) {
	var count int
	err := db.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM memories
		 WHERE created_at >= NOW() - INTERVAL '%d hours'
		   AND salience >= $1
		   AND memory_type NOT IN (%s)
		   AND COALESCE(source, '') != 'backfill'`, hours, FormatSQLExclusion(ExcludeSynthetic)),
		minSalience,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count recent memories: %w", err)
	}
	return count, nil
}

// RecentMemory holds a single memory from a recent-memories query.
type RecentMemory struct {
	Summary   string
	Salience  float64
}

// QueryRecentMemories returns recent non-superseded, non-backfill memories
// with salience >= minSalience, ordered by salience DESC then created_at DESC.
func QueryRecentMemories(ctx context.Context, db *pgxpool.Pool, hours int, minSalience float64, limit int) ([]RecentMemory, error) {
	rows, err := db.Query(ctx,
		fmt.Sprintf(`SELECT summary, salience FROM memories
		 WHERE created_at >= NOW() - INTERVAL '%d hours'
		   AND salience >= $1
		   AND memory_type NOT IN (%s)
		   AND superseded_by IS NULL
		   AND COALESCE(source, '') != 'backfill'
		 ORDER BY salience DESC, created_at DESC
		 LIMIT %d`, hours, FormatSQLExclusion(ExcludeSynthetic), limit),
		minSalience,
	)
	if err != nil {
		return nil, fmt.Errorf("query recent memories: %w", err)
	}
	defer rows.Close()

	var result []RecentMemory
	for rows.Next() {
		var m RecentMemory
		if rows.Scan(&m.Summary, &m.Salience) == nil {
			result = append(result, m)
		}
	}
	return result, nil
}

// HighSalienceMemory pairs a memory ID with its summary text.
type HighSalienceMemory struct {
	ID      int
	Summary string
}

// QueryHighSalienceMemories returns memories with salience >= threshold
// created in the last 24 hours, including their IDs. Used by the
// consolidation pipeline for profile updates and memory merging.
// Excludes identity profiles (read separately via GetIdentityProfile).
func QueryHighSalienceMemories(ctx context.Context, db *pgxpool.Pool, threshold, limit int) ([]HighSalienceMemory, error) {
	rows, err := db.Query(ctx,
		`SELECT id, summary FROM memories
		 WHERE salience >= $1
		   AND superseded_by IS NULL
		   AND memory_type != 'identity'
		   AND created_at >= NOW() - INTERVAL '24 hours'
		 ORDER BY salience DESC, created_at DESC
		 LIMIT $2`,
		threshold, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query high-salience memories: %w", err)
	}
	defer rows.Close()

	var results []HighSalienceMemory
	for rows.Next() {
		var m HighSalienceMemory
		if err := rows.Scan(&m.ID, &m.Summary); err != nil {
			return nil, fmt.Errorf("scan high-salience row: %w", err)
		}
		results = append(results, m)
	}
	return results, rows.Err()
}

// HasNewMemoriesSinceConsolidation checks whether any non-consolidation
// memories with sufficient salience exist since the last consolidation run.
// Returns false when the only high-salience memories are prior consolidation
// outputs, preventing runaway re-consolidation on restart.
func HasNewMemoriesSinceConsolidation(ctx context.Context, db *pgxpool.Pool, threshold int) (bool, error) {
	var count int
	err := db.QueryRow(ctx, `
		WITH last_consol AS (
			SELECT COALESCE(MAX(created_at), '1970-01-01'::timestamptz) AS ts
			FROM memories
			WHERE source = 'consolidation' AND superseded_by IS NULL
		)
		SELECT count(*) FROM memories m, last_consol lc
		WHERE m.salience >= $1
		  AND m.superseded_by IS NULL
		  AND m.source IS DISTINCT FROM 'consolidation'
		  AND m.memory_type != 'identity'
		  AND m.created_at > lc.ts`, threshold).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check new memories since consolidation: %w", err)
	}
	return count > 0, nil
}

// QueryRecentSummaries returns summary strings for recent non-backfill memories
// with salience >= minSalience, ordered by created_at DESC.
func QueryRecentSummaries(ctx context.Context, db *pgxpool.Pool, hours int, minSalience float64, limit int) ([]string, error) {
	rows, err := db.Query(ctx,
		fmt.Sprintf(`SELECT summary FROM memories
		 WHERE created_at >= NOW() - INTERVAL '%d hours'
		   AND salience >= $1
		   AND memory_type NOT IN (%s)
		   AND COALESCE(source, '') != 'backfill'
		 ORDER BY created_at DESC
		 LIMIT %d`, hours, FormatSQLExclusion(ExcludeSynthetic), limit),
		minSalience,
	)
	if err != nil {
		return nil, fmt.Errorf("query recent summaries: %w", err)
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			result = append(result, s)
		}
	}
	return result, nil
}
