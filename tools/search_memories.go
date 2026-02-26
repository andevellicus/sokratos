package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/memory"
)

// RegexFilter adds a regex constraint to the WHERE clause. For example,
// FieldPrefix="From:" and Value="alice" matches rows where summary contains
// "From:.*alice".
type RegexFilter struct {
	FieldPrefix string // "From:", "Title:", etc.
	Value       string
}

// MemorySearchConfig parameterises SearchMemoriesByDomain for email, calendar,
// or any other domain-tagged search.
type MemorySearchConfig struct {
	DomainTag     string        // "email" / "calendar"
	Filters       []RegexFilter
	QueryText     string        // explicit query text
	FallbackParts []string      // joined if QueryText is empty
	MaxResults    int64
	StartDate     *time.Time    // optional: filter by source_date >= start
	EndDate       *time.Time    // optional: filter by source_date <= end
	ResultNoun    string        // "email" / "calendar event"
	ResultLabel   string        // "Email" / "Event" — used in "--- Email 1 ---"
	ErrorPrefix   string        // "Email search" / "Calendar search"
}

// SearchMemoriesByDomain runs a vector+BM25 hybrid search over memories tagged
// with the given domain, applying optional regex filters.
func SearchMemoriesByDomain(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel string, cfg MemorySearchConfig) (string, error) {
	// Build dynamic WHERE clause.
	where := []string{fmt.Sprintf("'%s' = ANY(tags)", cfg.DomainTag)}
	// $1 = embedding, $2 = query text, $3 = max_results
	var extraArgs []interface{}
	nextParam := 4

	// Date filtering uses COALESCE(source_date, created_at) so older memories
	// without source_date fall back to ingestion time.
	if cfg.StartDate != nil {
		where = append(where, fmt.Sprintf("COALESCE(source_date, created_at) >= $%d", nextParam))
		extraArgs = append(extraArgs, *cfg.StartDate)
		nextParam++
	}
	if cfg.EndDate != nil {
		where = append(where, fmt.Sprintf("COALESCE(source_date, created_at) <= $%d", nextParam))
		extraArgs = append(extraArgs, *cfg.EndDate)
		nextParam++
	}

	for _, f := range cfg.Filters {
		where = append(where, fmt.Sprintf("summary ~* $%d", nextParam))
		extraArgs = append(extraArgs, f.FieldPrefix+`.*`+regexp.QuoteMeta(f.Value))
		nextParam++
	}

	queryText := cfg.QueryText
	if queryText == "" {
		queryText = strings.Join(cfg.FallbackParts, " ")
	}

	emb, err := memory.GetEmbedding(ctx, embedEndpoint, embedModel, queryText)
	if err != nil {
		return fmt.Sprintf("Failed to embed query: %v", err), nil
	}

	sql := fmt.Sprintf(
		`SELECT id, summary FROM memories
		 WHERE %s
		 ORDER BY
		     (embedding <=> $1) / GREATEST(
		         %s * (1 + ts_rank(
		             to_tsvector('english', summary),
		             websearch_to_tsquery('english', regexp_replace($2, '\s+', ' or ', 'g'))
		         ) * 20),
		         1)
		 LIMIT $3`,
		strings.Join(where, " AND "),
		decaySQL,
	)

	allArgs := []interface{}{pgvector.NewVector(emb), queryText, cfg.MaxResults}
	allArgs = append(allArgs, extraArgs...)

	rows, err := pool.Query(ctx, sql, allArgs...)
	if err != nil {
		return fmt.Sprintf("%s failed: %v", cfg.ErrorPrefix, err), nil
	}
	defer rows.Close()

	var ids []int64
	var results []string
	for rows.Next() {
		var id int64
		var summary string
		if err := rows.Scan(&id, &summary); err != nil {
			continue
		}
		ids = append(ids, id)
		results = append(results, summary)
	}

	if len(results) == 0 {
		return fmt.Sprintf("No %ss found matching your query.", cfg.ResultNoun), nil
	}

	trackRetrieval(pool, ids)

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d %s(s):\n\n", len(results), cfg.ResultNoun)
	for i, r := range results {
		fmt.Fprintf(&b, "--- %s %d ---\n%s\n\n", cfg.ResultLabel, i+1, r)
	}
	return b.String(), nil
}
