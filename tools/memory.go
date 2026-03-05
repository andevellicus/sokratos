package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/clients"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/timefmt"
	"sokratos/tokens"
)

// --- Search tool ---

// FlexibleStringSlice accepts both a JSON string ("email") and an array
// (["email"]) during unmarshalling. LLMs frequently emit a bare string
// instead of a single-element array for the tags field.
type FlexibleStringSlice []string

func (f *FlexibleStringSlice) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*f = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*f = []string{s}
	return nil
}

type searchMemoryArgs struct {
	Query      string              `json:"query"`
	Tags       FlexibleStringSlice `json:"tags,omitempty"`
	StartDate  string              `json:"start_date,omitempty"` // ISO8601
	EndDate    string              `json:"end_date,omitempty"`   // ISO8601
	MemoryType string              `json:"memory_type,omitempty"`
}

const rewriteSystemPrompt = `You are a search query optimizer for a personal memory database. Given the user's query, output exactly 3 alternative search queries that would help retrieve relevant memories.

Rules:
- Preserve the specific people, places, topics, and timeframes mentioned in the original query.
- Vary the phrasing and word choice to maximize semantic coverage (synonyms, related terms).
- Keep each query concise (under 15 words).
- Output ONLY the 3 queries, one per line. No numbering, no preamble, no explanation.`

const rerankSystemPrompt = `You are a search result re-ranker. Given a query and a numbered list of memory summaries, output the numbers of the most relevant results in order of relevance.

Rules:
- Only include results that DIRECTLY relate to the query topic.
- Be selective: return at most 5-6 results. Most queries should return 3-5.
- Exclude results about unrelated personal topics unless the query asks about them.
- Output ONLY the numbers, one per line, no explanation.`

// perEmbeddingLimit is the number of results to retrieve per embedding query.
const perEmbeddingLimit = 5

// searchResult holds a single deduplication-ready memory row.
type searchResult struct {
	id        int64
	summary   string
	createdAt time.Time
	score     float64 // 1 - cosine_distance
}

// rewriteQuery calls the subagent to produce up to 3 concise reformulations
// of the user's query. Returns nil on failure (caller falls back to the
// original query).
func rewriteQuery(ctx context.Context, sc *clients.SubagentClient, query string) []string {
	rewriteCtx, cancel := context.WithTimeout(ctx, TimeoutSubagentCall)
	defer cancel()

	content, err := sc.Complete(rewriteCtx, rewriteSystemPrompt, query, tokens.QueryRewrite)
	if err != nil {
		logger.Log.Warnf("[memory] query rewrite failed: %v", err)
		return nil
	}

	var variations []string
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			variations = append(variations, line)
			if len(variations) >= 3 {
				break
			}
		}
	}
	return variations
}

// contentHash returns a hex-encoded SHA-256 digest of the summary text,
// used as a deduplication key across multiple embedding queries.
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// retrievalOrderBy is the composite ranking expression used by all retrieval
// queries. Lower values = better. Combines cosine distance, BM25 full-text
// boost, salience, usefulness (weight 0.15), temporal decay, confidence,
// log-dampened retrieval popularity, and entity exact-match boost.
// $3 = embedding vector, $4 = raw query text for ts_rank + entity matching.
var retrievalOrderBy = memory.RankingOrderBy(3, 4)

// queryMemories runs the vector search SQL for a single embedding vector.
// Only non-superseded memories are returned. queryText is the raw search
// string used for BM25 full-text boosting via ts_rank. Optional tags and
// memoryType add additional WHERE filters with dynamic parameter positions.
func queryMemories(ctx context.Context, pool *pgxpool.Pool, emb []float32, queryText string, startDate, endDate *time.Time, tags []string, memoryType string) ([]searchResult, error) {
	// Fixed params: $1=startDate, $2=endDate, $3=embedding, $4=queryText
	args := []any{startDate, endDate, pgvector.NewVector(emb), queryText}
	nextParam := 5

	// Build dynamic WHERE clause additions.
	var extraWhere string
	if len(tags) > 0 {
		extraWhere += fmt.Sprintf("\n\t            AND tags && $%d", nextParam)
		args = append(args, tags)
		nextParam++
	}
	if memoryType != "" {
		extraWhere += fmt.Sprintf("\n\t            AND memory_type = $%d", nextParam)
		args = append(args, memoryType)
	} else {
		// Exclude internal memory types from general searches.
		extraWhere += "\n\t            AND memory_type NOT IN (" + memory.FormatSQLExclusion(memory.ExcludeInternal) + ")"
	}

	query := fmt.Sprintf(`SELECT id, summary, created_at,
	                 1 - (embedding <=> $3) AS score
	          FROM memories
	          WHERE superseded_by IS NULL
	            AND created_at >= COALESCE($1::timestamptz, '-infinity'::timestamptz)
	            AND created_at <= COALESCE($2::timestamptz, 'infinity'::timestamptz)%s
	          ORDER BY %s
	          LIMIT 5`, extraWhere, retrievalOrderBy)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []searchResult
	for rows.Next() {
		var r searchResult
		if err := rows.Scan(&r.id, &r.summary, &r.createdAt, &r.score); err != nil {
			return nil, fmt.Errorf("scan memory row: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// searchMemory implements multi-embedding retrieve, deduplicate, and re-rank:
//  1. Rewrite query via subagent → up to 3 variations
//  2. Embed original + variations → up to 4 embeddings
//  3. Query DB per embedding (LIMIT 5 each), deduplicate by content hash
//  4. Re-rank candidates via subagent (if available and candidates > limit)
//  5. Return top results sorted by relevance
func searchMemory(ctx context.Context, args json.RawMessage, pool *pgxpool.Pool, embedEndpoint, embedModel string, subagent *clients.SubagentClient, limit int) (string, error) {
	if limit <= 0 {
		limit = 3
	}
	a, err := ParseArgs[searchMemoryArgs](args)
	if err != nil {
		return err.Error(), nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query must not be empty")
	}

	// Parse optional temporal bounds. nil → COALESCE falls through to ±infinity.
	var startDate, endDate *time.Time
	if a.StartDate != "" {
		t, err := timefmt.ParseISO8601(a.StartDate)
		if err != nil {
			return fmt.Sprintf("invalid start_date %q: %v", a.StartDate, err), nil
		}
		startDate = &t
	}
	if a.EndDate != "" {
		t, err := timefmt.ParseISO8601(a.EndDate)
		if err != nil {
			return fmt.Sprintf("invalid end_date %q: %v", a.EndDate, err), nil
		}
		endDate = &t
	}

	// --- Query rewriting ---
	queries := []string{a.Query}
	if subagent != nil {
		if variations := rewriteQuery(ctx, subagent, a.Query); len(variations) > 0 {
			queries = append(queries, variations...)
		}
	}

	// --- Multi-embedding retrieval ---
	best := make(map[string]searchResult) // content hash → best result
	var lastErr error
	successCount := 0

	for _, q := range queries {
		emb, err := memory.GetEmbedding(ctx, embedEndpoint, embedModel, q)
		if err != nil {
			logger.Log.Warnf("[memory] embedding failed for query %q: %v", q, err)
			lastErr = err
			continue
		}

		results, err := queryMemories(ctx, pool, emb, a.Query, startDate, endDate, a.Tags, a.MemoryType)
		if err != nil {
			logger.Log.Warnf("[memory] query failed for %q: %v", q, err)
			lastErr = err
			continue
		}
		successCount++

		for _, r := range results {
			h := contentHash(r.summary)
			if existing, ok := best[h]; !ok || r.score > existing.score {
				best[h] = r
			}
		}
	}

	// Error contract: only fail if ALL variations failed.
	if successCount == 0 {
		return "", fmt.Errorf("all query variations failed: %w", lastErr)
	}

	if len(best) == 0 {
		return "No relevant memories found.", nil
	}

	// --- Entity graph multi-hop recall ---
	// Find memories sharing entities with the initial results but not already
	// retrieved. Uses the existing memories_entities_idx GIN index.
	initialIDs := make([]int64, 0, len(best))
	for _, r := range best {
		initialIDs = append(initialIDs, r.id)
	}
	// Use the first successful embedding for scoring hop results.
	var primaryEmb []float32
	for _, q := range queries {
		emb, err := memory.GetEmbedding(ctx, embedEndpoint, embedModel, q)
		if err == nil {
			primaryEmb = emb
			break
		}
	}
	if primaryEmb != nil {
		hopRows, hopErr := pool.Query(ctx,
			`SELECT DISTINCT m2.id, m2.summary, m2.created_at,
			        1 - (m2.embedding <=> $2) AS score
			 FROM memories m1
			 JOIN memories m2 ON m1.entities && m2.entities AND m1.id != m2.id
			 WHERE m1.id = ANY($1)
			   AND m2.superseded_by IS NULL
			   AND m2.id != ALL($1)
			 ORDER BY score DESC
			 LIMIT 3`,
			initialIDs, pgvector.NewVector(primaryEmb),
		)
		if hopErr != nil {
			logger.Log.Warnf("[memory] entity hop query failed: %v", hopErr)
		} else {
			for hopRows.Next() {
				var r searchResult
				if err := hopRows.Scan(&r.id, &r.summary, &r.createdAt, &r.score); err != nil {
					continue
				}
				// Apply 0.8x penalty so hop results don't dominate direct matches.
				r.score *= 0.8
				h := contentHash(r.summary)
				if existing, ok := best[h]; !ok || r.score > existing.score {
					best[h] = r
				}
			}
			hopRows.Close()
		}
	}

	// Collect, sort by score descending.
	results := make([]searchResult, 0, len(best))
	for _, r := range best {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	// Re-rank via subagent if available and we have more candidates than limit.
	if subagent != nil && len(results) > limit {
		results = rerankResults(ctx, subagent, a.Query, results, limit)
	}
	if len(results) > limit {
		results = results[:limit]
	}

	// Track retrieval: increment count, reset timer, dampened salience bump.
	ids := make([]int64, len(results))
	for i, r := range results {
		ids[i] = r.id
	}
	trackRetrieval(pool, ids)

	// Format output.
	var b strings.Builder
	b.WriteString("[MEMORY RETRIEVAL RESULTS]:\n")
	for _, r := range results {
		fmt.Fprintf(&b, "--- (score: %.2f, recorded: %s)\n", r.score, timefmt.FormatDate(r.createdAt))
		b.WriteString(memory.ExtractSummary(r.summary))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// rerankResults calls the subagent to re-rank candidates by relevance to the
// query. Returns up to limit results in preferred order. Falls back to the
// original ordering on any failure.
func rerankResults(ctx context.Context, sc *clients.SubagentClient, query string, results []searchResult, limit int) []searchResult {
	// Build a numbered list of summaries for the re-ranker.
	var sb strings.Builder
	fmt.Fprintf(&sb, "Query: %s\n\nResults:\n", query)
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, r.summary)
	}

	rerankCtx, cancel := context.WithTimeout(ctx, TimeoutSubagentCall)
	defer cancel()

	content, err := sc.Complete(rerankCtx, rerankSystemPrompt, sb.String(), tokens.Rerank)
	if err != nil {
		logger.Log.Warnf("[memory] re-ranking failed, using original order: %v", err)
		return results
	}

	// Parse numbered output: each line should be a 1-based index.
	var reranked []searchResult
	seen := make(map[int]bool)
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		line = strings.TrimSpace(line)
		idx, err := strconv.Atoi(line)
		if err != nil || idx < 1 || idx > len(results) || seen[idx] {
			continue
		}
		seen[idx] = true
		reranked = append(reranked, results[idx-1])
		if len(reranked) >= limit {
			break
		}
	}

	if len(reranked) == 0 {
		logger.Log.Warnf("[memory] re-ranking produced no valid indices, using original order")
		return results
	}

	logger.Log.Debugf("[memory] re-ranked %d → %d results", len(results), len(reranked))
	return reranked
}

// trackRetrieval bumps retrieval stats on a background goroutine.
// Delegates to memory.TrackRetrieval for the actual SQL.
func trackRetrieval(pool *pgxpool.Pool, ids []int64) {
	if len(ids) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutRetrievalTracking)
		defer cancel()
		memory.TrackRetrieval(ctx, pool, ids)
	}()
}

// --- Save tool (async) ---

type saveMemoryArgs struct {
	Summary       string              `json:"summary"`
	Tags          FlexibleStringSlice `json:"tags"`
	Category      string              `json:"category"`              // prepended to tags
	SalienceScore *int                `json:"salience_score"`        // 0-10 integer scale
	MemoryType    string              `json:"memory_type,omitempty"` // general, fact, preference, event
}

// effectiveSalience returns the salience on the 0-10 scale, defaulting to 5.
func (a saveMemoryArgs) effectiveSalience() float64 {
	if a.SalienceScore != nil {
		s := *a.SalienceScore
		if s < 0 {
			s = 0
		} else if s > 10 {
			s = 10
		}
		return float64(s)
	}
	return 5
}

// effectiveTags returns tags with category prepended if set.
func (a saveMemoryArgs) effectiveTags() []string {
	if a.Category == "" {
		return a.Tags
	}
	return append([]string{a.Category}, a.Tags...)
}

// saveMemoryAsync embeds, contradiction-checks (when bgGrammarFn is available),
// and inserts the memory on a background goroutine so it doesn't block the
// calling agent. Falls back to ScoreAndWrite when contradiction detection
// dependencies are unavailable.
func saveMemoryAsync(pool *pgxpool.Pool, embedEndpoint, embedModel string,
	bgGrammarFn memory.GrammarSubagentFunc, grammarFn memory.GrammarSubagentFunc, queueFn memory.WorkQueueFunc,
	a saveMemoryArgs) {

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutMemorySave)
		defer cancel()

		req := memory.MemoryWriteRequest{
			Summary:       a.Summary,
			Tags:          a.effectiveTags(),
			Salience:      a.effectiveSalience(),
			MemoryType:    a.MemoryType,
			Source:        "user",
			EmbedEndpoint: embedEndpoint,
			EmbedModel:    embedModel,
		}

		var id int64
		var err error
		if bgGrammarFn != nil {
			id, err = memory.CheckAndWriteWithContradiction(ctx, pool, req, bgGrammarFn, queueFn)
		} else {
			id, err = memory.ScoreAndWrite(ctx, pool, req, grammarFn, queueFn)
		}
		if err != nil {
			logger.Log.Errorf("[save_memory] failed: %v", err)
			return
		}
		logger.Log.Infof("[save_memory] saved id=%d (salience=%.0f, tags=%v): %s", id, req.Salience, req.Tags, a.Summary)
	}()
}

// --- Registry wiring ---

// NewSearchMemory returns a ToolFunc that closes over the pool, endpoints, and subagent.
func NewSearchMemory(pool *pgxpool.Pool, embedEndpoint, embedModel string, subagent *clients.SubagentClient, limit int) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		return searchMemory(ctx, args, pool, embedEndpoint, embedModel, subagent, limit)
	}
}

// NewSaveMemory returns a ToolFunc that closes over the pool, endpoints, and
// subagent functions. When bgGrammarFn is non-nil, saves go through
// contradiction detection so the orchestrator can't accidentally overwrite
// corrected facts. When backends are busy, contradiction checks and entity
// extraction are deferred to the work queue.
func NewSaveMemory(pool *pgxpool.Pool, embedEndpoint, embedModel string,
	bgGrammarFn memory.GrammarSubagentFunc, grammarFn memory.GrammarSubagentFunc, queueFn memory.WorkQueueFunc) ToolFunc {

	return func(_ context.Context, args json.RawMessage) (string, error) {
		a, err := ParseArgs[saveMemoryArgs](args)
		if err != nil {
			return err.Error(), nil
		}
		summary := strings.TrimSpace(a.Summary)
		if summary == "" || strings.HasPrefix(summary, "<No ") || strings.HasPrefix(summary, "<no ") {
			return "error: summary is required and must contain actual content", nil
		}

		saveMemoryAsync(pool, embedEndpoint, embedModel, bgGrammarFn, grammarFn, queueFn, a)
		return "Memory queued for saving.", nil
	}
}
