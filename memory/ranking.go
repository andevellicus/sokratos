package memory

import "fmt"

// RankingOrderBy returns the composite ranking ORDER BY expression used for
// memory retrieval. Lower values = better. Combines:
//   - Cosine distance (primary signal)
//   - BM25 full-text boost via ts_rank
//   - Salience (decayed, 0-10 scale)
//   - Usefulness score (weight 0.15)
//   - Temporal recency bias (~30-day half-life on creation date)
//   - Confidence boost (higher confidence → lower rank value)
//   - Log-dampened retrieval popularity
//   - Entity exact-match boost (textParam matches entities array)
//
// embParam is the $N position for the embedding vector parameter.
// textParam is the $N position for the raw query text parameter (used for
// BM25 ts_rank and entity matching).
func RankingOrderBy(embParam, textParam int) string {
	return fmt.Sprintf(
		`(embedding <=> $%d)
	/ GREATEST(1.0 + ts_rank(summary_tsv, websearch_to_tsquery('english', regexp_replace($%d, '\s+', ' or ', 'g'))) * 10, 1.0)
	- (COALESCE(salience, 5) * 0.1)
	- (COALESCE(usefulness_score, 0.5) * 0.15)
	- (0.03 * EXTRACT(EPOCH FROM (NOW() - created_at)) / 86400 / 30)
	- (COALESCE(confidence, 1.0) * 0.03)
	- (ln(COALESCE(retrieval_count, 0) + 1) * 0.02)
	- (CASE WHEN $%d = ANY(entities) THEN 0.2 ELSE 0 END)`,
		embParam, textParam, textParam,
	)
}
