package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
)

// --- Embedding client ---

type embeddingReq struct {
	Input any    `json:"input"` // string or []string for batch
	Model string `json:"model"`
}

type embeddingResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

var embeddingHTTPClient = &http.Client{Timeout: TimeoutEmbeddingCall}

// GetEmbedding calls an OpenAI-compatible /v1/embeddings endpoint and returns the vector.
func GetEmbedding(ctx context.Context, endpoint string, model string, text string) ([]float32, error) {
	body, err := json.Marshal(embeddingReq{Input: text, Model: model})
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := embeddingHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embedding server returned status %d: %s", resp.StatusCode, bytes.TrimSpace(errBody))
	}

	var raw embeddingResp
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}

	if len(raw.Data) == 0 {
		return nil, fmt.Errorf("embedding server returned empty data array")
	}

	return raw.Data[0].Embedding, nil
}

// GetEmbeddings calls an OpenAI-compatible /v1/embeddings endpoint with an
// array of texts and returns all embedding vectors in one request.
func GetEmbeddings(ctx context.Context, endpoint string, model string, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embeddingReq{Input: texts, Model: model})
	if err != nil {
		return nil, fmt.Errorf("marshal batch embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create batch embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := embeddingHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embedding server returned status %d: %s", resp.StatusCode, bytes.TrimSpace(errBody))
	}

	var raw embeddingResp
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode batch embedding response: %w", err)
	}

	if len(raw.Data) != len(texts) {
		return nil, fmt.Errorf("embedding server returned %d vectors for %d inputs", len(raw.Data), len(texts))
	}

	result := make([][]float32, len(raw.Data))
	for i, d := range raw.Data {
		result[i] = d.Embedding
	}
	return result, nil
}

// embeddedChunk pairs a text fragment with its embedding vector.
type embeddedChunk struct {
	Text      string
	Embedding []float32
}

// embedWithFallback embeds text, recursively splitting in half on "too large"
// errors from the embedding server. Returns one or more (text, embedding)
// pairs. The minimum split size is 100 bytes to prevent infinite recursion.
func embedWithFallback(ctx context.Context, endpoint, model, text string) ([]embeddedChunk, error) {
	emb, err := GetEmbedding(ctx, endpoint, model, text)
	if err != nil {
		if strings.Contains(err.Error(), "too large to process") && len(text) > 100 {
			mid := len(text) / 2
			if nl := strings.LastIndex(text[:mid], "\n"); nl > 0 {
				mid = nl + 1
			}
			left, err := embedWithFallback(ctx, endpoint, model, strings.TrimSpace(text[:mid]))
			if err != nil {
				return nil, err
			}
			right, err := embedWithFallback(ctx, endpoint, model, strings.TrimSpace(text[mid:]))
			if err != nil {
				return nil, err
			}
			return append(left, right...), nil
		}
		return nil, err
	}
	return []embeddedChunk{{Text: text, Embedding: emb}}, nil
}

// MaxChunkBytes is the approximate byte limit per embedding chunk.
// BGE-large-en-v1.5 has a 512-token context. WordPiece tokenization
// ranges from ~4 bytes/token (plain English) down to ~2.8 bytes/token
// for structured/HTML content. 1200 bytes stays safely under the
// 512-token hard limit even for dense email content (~425 tokens at
// worst case).
const MaxChunkBytes = 1200

// SaveToMemoryAsync embeds content and inserts it into the memories table
// on a background goroutine so it doesn't block the caller. Content that
// exceeds the embedding model's context window is split into chunks.
func SaveToMemoryAsync(db *pgxpool.Pool, embedEndpoint, embedModel, tag, content string) {
	SaveToMemoryWithSalienceAsync(db, embedEndpoint, embedModel, content, 3.0, []string{tag}, "conversation_archive", nil)
}

// SaveToMemoryWithSalienceAsync is like SaveToMemoryAsync but accepts a custom
// salience score and tags instead of using defaults.
func SaveToMemoryWithSalienceAsync(db *pgxpool.Pool, embedEndpoint, embedModel, summary string, salience float64, tags []string, source string, sourceDate *time.Time) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutSaveOp)
		defer cancel()
		if err := SaveToMemoryWithSalience(ctx, db, embedEndpoint, embedModel, summary, salience, tags, source, sourceDate); err != nil {
			logger.Log.Errorf("[memory] async save failed: %v", err)
		}
	}()
}

// SaveToMemoryWithSalience is the synchronous version of SaveToMemoryWithSalienceAsync.
// It embeds and inserts each chunk sequentially, returning the first error encountered.
func SaveToMemoryWithSalience(ctx context.Context, db *pgxpool.Pool, embedEndpoint, embedModel, summary string, salience float64, tags []string, source string, sourceDate *time.Time) error {
	chunks := ChunkText(summary, MaxChunkBytes)
	for _, chunk := range chunks {
		embedded, err := embedWithFallback(ctx, embedEndpoint, embedModel, chunk)
		if err != nil {
			return fmt.Errorf("embedding failed: %w", err)
		}

		for _, ec := range embedded {
			_, err = db.Exec(ctx,
				"INSERT INTO memories (summary, embedding, salience, tags, source, source_date) VALUES ($1, $2, $3, $4, $5, $6)",
				ec.Text, pgvector.NewVector(ec.Embedding), salience, tags, source, sourceDate,
			)
			if err != nil {
				return fmt.Errorf("insert failed: %w", err)
			}

			logger.Log.Infof("[memory] archived %d bytes (salience=%.0f, tags=%v, source=%s)", len(ec.Text), salience, tags, source)
		}
	}
	return nil
}

// RecordMemoryUsefulness applies a dampened adjustment to usefulness_score.
// When useful: score + 0.1 * (1 - score) — pushes toward 1.0.
// When not useful: score - 0.1 * score — pushes toward 0.0.
func RecordMemoryUsefulness(ctx context.Context, db *pgxpool.Pool, memoryIDs []int64, wasUseful bool) {
	if len(memoryIDs) == 0 {
		return
	}
	var query string
	if wasUseful {
		query = `UPDATE memories
			SET usefulness_score = LEAST(COALESCE(usefulness_score, 0.5) + (0.1 * (1.0 - COALESCE(usefulness_score, 0.5))), 1.0)
			WHERE id = ANY($1)`
	} else {
		query = `UPDATE memories
			SET usefulness_score = GREATEST(COALESCE(usefulness_score, 0.5) - (0.1 * COALESCE(usefulness_score, 0.5)), 0.0)
			WHERE id = ANY($1)`
	}
	if _, err := db.Exec(ctx, query, memoryIDs); err != nil {
		logger.Log.Warnf("[memory] failed to record usefulness: %v", err)
	}
}

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

// --- Structured writes with quality scoring ---

// GraniteFunc calls a Granite/utility model with system + user prompts and
// returns the response text. The caller creates this as a closure over the
// actual LLM client, keeping the memory package free of llm/tools imports.
type GraniteFunc func(ctx context.Context, systemPrompt, userPrompt string) (string, error)

// MemoryWriteRequest describes a memory to be quality-scored and written.
type MemoryWriteRequest struct {
	Summary        string
	Tags           []string
	Salience       float64
	MemoryType     string     // "general", "fact", "preference", "event", "email", "calendar"
	Source         string     // provenance: "conversation", "email", "calendar", "user", "backfill", "conversation_archive"
	SourceDate     *time.Time // original date of the source item (email sent date, event start, etc.)
	EmbedEndpoint  string
	EmbedModel     string
}

// qualityResult is the parsed response from Granite quality scoring.
type qualityResult struct {
	Specificity float64  `json:"specificity"` // 0-1: how specific/actionable
	Uniqueness  float64  `json:"uniqueness"`  // 0-1: how novel vs existing info
	Entities    []string `json:"entities"`    // extracted named entities
	Confidence  float64  `json:"confidence"`  // 0-1: factual confidence
}

const qualitySystemPrompt = `You are a memory quality scorer. Given a memory summary, evaluate it and return a JSON object:
{"specificity": 0.0-1.0, "uniqueness": 0.0-1.0, "entities": ["entity1", "entity2"], "confidence": 0.0-1.0}

- specificity: How specific and actionable is this information? (0=vague, 1=precise facts)
- uniqueness: How novel is this information likely to be? (0=common knowledge, 1=highly personal/unique)
- entities: Extract named entities (people, places, organizations, dates, products)
- confidence: How factually confident is this statement? (0=uncertain, 1=definitive)

Return ONLY the JSON object. No explanation.`

// ScoreAndWrite quality-scores a memory via Granite, embeds it, and inserts it.
// Returns the new memory ID. If Granite scoring fails, the memory is still saved
// with default quality values (graceful degradation).
func ScoreAndWrite(ctx context.Context, db *pgxpool.Pool, req MemoryWriteRequest, granite GraniteFunc) (int64, error) {
	// Quality scoring via Granite (best-effort).
	var entities []string
	confidence := 1.0
	if granite != nil {
		scoreCtx, cancel := context.WithTimeout(ctx, TimeoutQualityScore)
		raw, err := granite(scoreCtx, qualitySystemPrompt, req.Summary)
		cancel()
		if err != nil {
			logger.Log.Warnf("[memory] quality scoring failed, using defaults: %v", err)
		} else {
			var qr qualityResult
			if err := json.Unmarshal([]byte(raw), &qr); err != nil {
				logger.Log.Warnf("[memory] quality JSON parse failed: %v (raw: %s)", err, raw)
			} else {
				entities = qr.Entities
				confidence = qr.Confidence
				// Adjust salience based on quality: boost specific+unique memories.
				qualityBoost := (qr.Specificity + qr.Uniqueness) / 2.0
				req.Salience = req.Salience + (qualityBoost * (1.0 - req.Salience/10.0))
				if req.Salience > 10 {
					req.Salience = 10
				}
			}
		}
	}

	memType := req.MemoryType
	if memType == "" {
		memType = "general"
	}

	// Chunk long content; first chunk reuses scored metadata.
	chunks := ChunkText(req.Summary, MaxChunkBytes)
	var firstID int64

	for i, chunk := range chunks {
		embedded, err := embedWithFallback(ctx, req.EmbedEndpoint, req.EmbedModel, chunk)
		if err != nil {
			return firstID, fmt.Errorf("embedding chunk %d failed: %w", i, err)
		}

		for _, ec := range embedded {
			var id int64
			err = db.QueryRow(ctx,
				`INSERT INTO memories (summary, embedding, salience, tags, memory_type, entities, confidence, source, source_date)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 RETURNING id`,
				ec.Text, pgvector.NewVector(ec.Embedding), req.Salience, req.Tags,
				memType, entities, confidence, req.Source, req.SourceDate,
			).Scan(&id)
			if err != nil {
				return firstID, fmt.Errorf("insert chunk %d failed: %w", i, err)
			}

			if firstID == 0 {
				firstID = id
			}
			logger.Log.Infof("[memory] scored+saved id=%d chunk=%d/%d (salience=%.1f, entities=%v, confidence=%.2f, source=%s): %s",
				id, i+1, len(chunks), req.Salience, entities, confidence, req.Source, ec.Text)
		}
	}

	return firstID, nil
}

// --- Contradiction detection ---

const contradictionSystemPrompt = `You check whether a new memory contradicts an existing one. Given:
- NEW: the new memory
- EXISTING: an existing memory

If the new memory directly contradicts the existing one (e.g. updated facts, changed preferences, corrected information), respond with exactly:
CONTRADICTS

If they are compatible or unrelated, respond with exactly:
COMPATIBLE

Respond with ONLY one of those two words. No explanation.`

// contradictionCandidate holds a potential contradiction match.
type contradictionCandidate struct {
	ID      int64
	Summary string
}

// CheckAndWriteWithContradiction finds similar existing memories, checks for
// contradictions via Granite, supersedes contradicted memories, and writes
// the new memory. Returns the new memory ID.
func CheckAndWriteWithContradiction(ctx context.Context, db *pgxpool.Pool, req MemoryWriteRequest, granite GraniteFunc) (int64, error) {
	// Chunk first; use first chunk for contradiction search embedding.
	chunks := ChunkText(req.Summary, MaxChunkBytes)

	// Embed the first chunk with fallback — needed for both similarity search and insert.
	firstEmbedded, err := embedWithFallback(ctx, req.EmbedEndpoint, req.EmbedModel, chunks[0])
	if err != nil {
		return 0, fmt.Errorf("embedding failed: %w", err)
	}
	// Use the first sub-chunk's embedding for contradiction search.
	firstEmb := firstEmbedded[0].Embedding

	// Find top 3 most similar non-superseded memories.
	var supersededIDs []int64
	rows, err := db.Query(ctx,
		`SELECT id, summary FROM memories
		 WHERE superseded_by IS NULL
		   AND (embedding <=> $1) < 0.3
		 ORDER BY (embedding <=> $1)
		 LIMIT 3`,
		pgvector.NewVector(firstEmb),
	)
	if err != nil {
		logger.Log.Warnf("[memory] contradiction search failed: %v", err)
	} else {
		var candidates []contradictionCandidate
		for rows.Next() {
			var c contradictionCandidate
			if err := rows.Scan(&c.ID, &c.Summary); err != nil {
				continue
			}
			candidates = append(candidates, c)
		}
		rows.Close()

		// Check each candidate for contradiction via Granite.
		if granite != nil {
			for _, c := range candidates {
				checkCtx, cancel := context.WithTimeout(ctx, TimeoutContradictionCheck)
				prompt := fmt.Sprintf("NEW: %s\nEXISTING: %s", req.Summary, c.Summary)
				raw, gErr := granite(checkCtx, contradictionSystemPrompt, prompt)
				cancel()
				if gErr != nil {
					logger.Log.Warnf("[memory] contradiction check failed for id=%d: %v", c.ID, gErr)
					continue
				}
				if strings.TrimSpace(strings.ToUpper(raw)) == "CONTRADICTS" {
					logger.Log.Infof("[memory] new memory contradicts id=%d, will supersede", c.ID)
					supersededIDs = append(supersededIDs, c.ID)
				}
			}
		}
	}

	// Quality scoring via Granite (best-effort) — score once, apply to all chunks.
	var entities []string
	confidence := 1.0
	if granite != nil {
		scoreCtx, cancel := context.WithTimeout(ctx, TimeoutQualityScore)
		raw, gErr := granite(scoreCtx, qualitySystemPrompt, req.Summary)
		cancel()
		if gErr != nil {
			logger.Log.Warnf("[memory] quality scoring failed, using defaults: %v", gErr)
		} else {
			var qr qualityResult
			if err := json.Unmarshal([]byte(raw), &qr); err != nil {
				logger.Log.Warnf("[memory] quality JSON parse failed: %v (raw: %s)", err, raw)
			} else {
				entities = qr.Entities
				confidence = qr.Confidence
				qualityBoost := (qr.Specificity + qr.Uniqueness) / 2.0
				req.Salience = req.Salience + (qualityBoost * (1.0 - req.Salience/10.0))
				if req.Salience > 10 {
					req.Salience = 10
				}
			}
		}
	}

	memType := req.MemoryType
	if memType == "" {
		memType = "general"
	}

	// Insert all chunks with same metadata. First chunk reuses pre-computed embeddings.
	var firstID int64
	for i, chunk := range chunks {
		var embedded []embeddedChunk
		if i == 0 {
			embedded = firstEmbedded
		} else {
			embedded, err = embedWithFallback(ctx, req.EmbedEndpoint, req.EmbedModel, chunk)
			if err != nil {
				return firstID, fmt.Errorf("embedding chunk %d failed: %w", i, err)
			}
		}

		for _, ec := range embedded {
			var id int64
			err = db.QueryRow(ctx,
				`INSERT INTO memories (summary, embedding, salience, tags, memory_type, entities, confidence, source, source_date)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 RETURNING id`,
				ec.Text, pgvector.NewVector(ec.Embedding), req.Salience, req.Tags,
				memType, entities, confidence, req.Source, req.SourceDate,
			).Scan(&id)
			if err != nil {
				return firstID, fmt.Errorf("insert chunk %d failed: %w", i, err)
			}

			if firstID == 0 {
				firstID = id
			}
			logger.Log.Infof("[memory] contradiction-checked+saved id=%d chunk=%d/%d (salience=%.1f, entities=%v, source=%s): %s",
				id, i+1, len(chunks), req.Salience, entities, req.Source, ec.Text)
		}
	}

	// Supersede contradicted memories by pointing them at the first chunk's ID.
	for _, oldID := range supersededIDs {
		if _, err := db.Exec(ctx,
			`UPDATE memories SET superseded_by = $1 WHERE id = $2`,
			firstID, oldID,
		); err != nil {
			logger.Log.Warnf("[memory] failed to supersede id=%d: %v", oldID, err)
		}
	}

	return firstID, nil
}

// ChunkText splits text into pieces of at most maxBytes, breaking at the
// last newline before the limit to avoid cutting mid-sentence.
func ChunkText(text string, maxBytes int) []string {
	if len(text) <= maxBytes {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		end := maxBytes
		if end > len(text) {
			end = len(text)
		}

		// Try to break at a newline boundary.
		if end < len(text) {
			if nl := strings.LastIndex(text[:end], "\n"); nl > 0 {
				end = nl + 1
			}
		}

		chunks = append(chunks, strings.TrimSpace(text[:end]))
		text = text[end:]
	}
	return chunks
}

// --- Identity Profile ---

// GetIdentityProfile returns the most recent identity profile from the memories
// table. Returns "{}" if no identity row exists (matching legacy file-not-found behavior).
func GetIdentityProfile(ctx context.Context, db *pgxpool.Pool) (string, error) {
	var summary string
	err := db.QueryRow(ctx,
		`SELECT summary FROM memories WHERE memory_type = 'identity' AND superseded_by IS NULL ORDER BY created_at DESC LIMIT 1`,
	).Scan(&summary)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return "{}", nil
		}
		return "{}", fmt.Errorf("get identity profile: %w", err)
	}
	return summary, nil
}

// WriteIdentityProfile inserts a new identity profile row into the memories
// table, superseding any previous identity rows. The profile is embedded for
// vector search and stored with salience 10 to prevent decay.
func WriteIdentityProfile(ctx context.Context, db *pgxpool.Pool, embedEndpoint, embedModel, profileJSON string) error {
	embedded, err := embedWithFallback(ctx, embedEndpoint, embedModel, "identity profile: "+profileJSON)
	if err != nil {
		return fmt.Errorf("embed identity profile: %w", err)
	}

	// Insert using the first sub-chunk's embedding (identity profiles are
	// stored as a single row regardless of embedding splits).
	var newID int64
	err = db.QueryRow(ctx,
		`INSERT INTO memories (summary, embedding, salience, tags, memory_type, source)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		profileJSON, pgvector.NewVector(embedded[0].Embedding), 10.0, []string{"identity", "profile"},
		"identity", "consolidation",
	).Scan(&newID)
	if err != nil {
		return fmt.Errorf("insert identity profile: %w", err)
	}

	// Supersede all previous identity rows.
	_, err = db.Exec(ctx,
		`UPDATE memories SET superseded_by = $1 WHERE memory_type = 'identity' AND superseded_by IS NULL AND id != $1`,
		newID,
	)
	if err != nil {
		return fmt.Errorf("supersede old identity profiles: %w", err)
	}

	logger.Log.Infof("[memory] identity profile written (id=%d, %d bytes)", newID, len(profileJSON))
	return nil
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

// --- Episodic Memory Synthesis ---

// EpisodeMemory holds the fields needed for similarity-based clustering.
type EpisodeMemory struct {
	ID        int64
	Summary   string
	Embedding []float32
}

const episodeSynthesisPrompt = `You synthesize related memories into a cohesive episodic narrative. Given a numbered list of related memories, produce a 2-4 sentence summary that:
- Captures the essential thread connecting these memories
- Preserves key facts, names, dates, and outcomes
- Reads as a natural narrative, not a bullet list

Return ONLY the narrative summary. No preamble, no explanation.`

// SynthesizeFunc is the function signature for calling an LLM to synthesize text.
type SynthesizeFunc func(ctx context.Context, systemPrompt, content string) (string, error)

// cosineSimilarity returns the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// clusterBySimilarity groups memories using greedy single-linkage clustering.
// Two memories are linked if their cosine distance < distanceThreshold
// (i.e., similarity > 1 - distanceThreshold).
func clusterBySimilarity(memories []EpisodeMemory, distanceThreshold float64) [][]EpisodeMemory {
	n := len(memories)
	assigned := make([]bool, n)
	var clusters [][]EpisodeMemory

	for i := 0; i < n; i++ {
		if assigned[i] {
			continue
		}
		cluster := []EpisodeMemory{memories[i]}
		assigned[i] = true

		for j := i + 1; j < n; j++ {
			if assigned[j] {
				continue
			}
			// Check if j is similar to any member of the current cluster.
			for _, member := range cluster {
				dist := 1.0 - cosineSimilarity(member.Embedding, memories[j].Embedding)
				if dist < distanceThreshold {
					cluster = append(cluster, memories[j])
					assigned[j] = true
					break
				}
			}
		}

		clusters = append(clusters, cluster)
	}
	return clusters
}

// SynthesizeEpisodes clusters recent memories by semantic similarity and
// synthesizes each cluster into an episodic memory. Returns the number of
// episodes created.
func SynthesizeEpisodes(ctx context.Context, db *pgxpool.Pool, embedEndpoint, embedModel string, synthesize SynthesizeFunc) (int, error) {
	// Query recent non-episode, non-reflection, non-superseded memories from last 24h.
	rows, err := db.Query(ctx,
		`SELECT id, summary, embedding
		 FROM memories
		 WHERE superseded_by IS NULL
		   AND memory_type NOT IN ('episode', 'reflection')
		   AND created_at >= now() - INTERVAL '24 hours'
		 ORDER BY created_at DESC
		 LIMIT 50`,
	)
	if err != nil {
		return 0, fmt.Errorf("query recent memories: %w", err)
	}
	defer rows.Close()

	var memories []EpisodeMemory
	for rows.Next() {
		var m EpisodeMemory
		var vec pgvector.Vector
		if err := rows.Scan(&m.ID, &m.Summary, &vec); err != nil {
			continue
		}
		m.Embedding = vec.Slice()
		memories = append(memories, m)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate recent memories: %w", err)
	}

	if len(memories) < 2 {
		return 0, nil
	}

	// Cluster by cosine similarity (threshold: distance < 0.3 = similarity > 0.7).
	clusters := clusterBySimilarity(memories, 0.3)

	episodeCount := 0
	for _, cluster := range clusters {
		if len(cluster) < 2 {
			continue
		}

		// Build numbered list for synthesis.
		var sb strings.Builder
		constituentIDs := make([]int64, len(cluster))
		for i, m := range cluster {
			constituentIDs[i] = m.ID
			fmt.Fprintf(&sb, "%d. %s\n", i+1, m.Summary)
		}

		// Synthesize episode narrative.
		raw, err := synthesize(ctx, episodeSynthesisPrompt, sb.String())
		if err != nil {
			logger.Log.Warnf("[memory] episode synthesis failed for cluster of %d: %v", len(cluster), err)
			continue
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		// Embed and insert the episode.
		embedded, err := embedWithFallback(ctx, embedEndpoint, embedModel, raw)
		if err != nil {
			logger.Log.Warnf("[memory] episode embedding failed: %v", err)
			continue
		}

		var episodeID int64
		for _, ec := range embedded {
			var id int64
			err = db.QueryRow(ctx,
				`INSERT INTO memories (summary, embedding, salience, tags, memory_type, source, related_ids)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)
				 RETURNING id`,
				ec.Text, pgvector.NewVector(ec.Embedding), 8.0, []string{"episode"},
				"episode", "episode_synthesis", constituentIDs,
			).Scan(&id)
			if err != nil {
				logger.Log.Warnf("[memory] episode insert failed: %v", err)
				break
			}
			if episodeID == 0 {
				episodeID = id
			}
		}
		if episodeID == 0 {
			continue
		}

		// Link constituent memories to the episode.
		for _, cID := range constituentIDs {
			_, _ = db.Exec(ctx,
				`UPDATE memories SET related_ids = array_append(COALESCE(related_ids, ARRAY[]::BIGINT[]), $1) WHERE id = $2`,
				episodeID, cID,
			)
		}

		episodeCount++
		logger.Log.Infof("[memory] synthesized episode id=%d from %d memories", episodeID, len(cluster))
	}

	return episodeCount, nil
}

// --- Reflection / Meta-Cognition ---

// ReflectOnMemories performs a meta-cognitive reflection over memories created
// since the given time, identifying patterns, evolving interests, connections,
// and predictions. Returns the reflection memory ID, or 0 if skipped.
func ReflectOnMemories(ctx context.Context, db *pgxpool.Pool, embedEndpoint, embedModel, reflectionPrompt string, synthesize SynthesizeFunc, since time.Time) (int64, error) {
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
			if len(summary) > 300 {
				summary = summary[:300]
			}
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
			`INSERT INTO memories (summary, embedding, salience, tags, memory_type, source)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 RETURNING id`,
			ec.Text, pgvector.NewVector(ec.Embedding), 9.0, []string{"reflection", "meta"},
			"reflection", "reflection",
		).Scan(&chunkID)
		if err != nil {
			return 0, fmt.Errorf("reflection insert failed: %w", err)
		}
		if id == 0 {
			id = chunkID
		}
	}

	logger.Log.Infof("[memory] reflection saved id=%d (%d bytes from %d memories)", id, len(raw), totalCount)
	return id, nil
}
