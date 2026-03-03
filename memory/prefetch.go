package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/timefmt"
)

// PrefetchResult holds the output of a prefetch: formatted XML context and
// the IDs of the memories that were matched.
type PrefetchResult struct {
	Content string  // formatted <retrieved_context> XML
	IDs     []int64 // matched memory IDs
}

// Prefetch embeds the given query, retrieves semantically similar memories
// using the hybrid ranking function, and formats them as an XML context block.
// Returns nil if embedding fails or no memories match.
func Prefetch(ctx context.Context, pool *pgxpool.Pool, embedURL, embedModel, query, fulltext string, limit int) *PrefetchResult {
	emb, err := GetEmbedding(ctx, embedURL, embedModel, query)
	if err != nil {
		if ctx.Err() != nil {
			logger.Log.Debugf("[prefetch] timed out, skipping")
		}
		return nil
	}

	rows, err := pool.Query(ctx,
		`SELECT id, summary, created_at
		 FROM memories
		 WHERE superseded_by IS NULL
		   AND memory_type NOT IN (` + FormatSQLExclusion(ExcludeInternal) + `)
		 ORDER BY `+RankingOrderBy(1, 2)+`
		 LIMIT `+fmt.Sprintf("%d", limit),
		pgvector.NewVector(emb), fulltext,
	)
	if err != nil {
		logger.Log.Warnf("[prefetch] db error: %v", err)
		return nil
	}
	defer rows.Close()

	var sb strings.Builder
	var ids []int64
	for rows.Next() {
		var id int64
		var summary string
		var createdAt time.Time
		if err := rows.Scan(&id, &summary, &createdAt); err != nil {
			continue
		}
		ids = append(ids, id)
		fmt.Fprintf(&sb, "- %s (recorded: %s)\n", ExtractSummary(summary), timefmt.FormatDate(createdAt))
	}
	if len(ids) == 0 {
		return nil
	}

	content := "<retrieved_context relevance=\"semantic\">\n" +
		"Potentially related memories. Use as background context only\n" +
		"if directly relevant. Do not force relevance if the connection is weak.\n" +
		sb.String() +
		"</retrieved_context>"
	logger.Log.Debugf("[prefetch] injected %d memories", len(ids))
	return &PrefetchResult{Content: content, IDs: ids}
}
