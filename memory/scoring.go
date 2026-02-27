package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
)

// --- Structured writes with quality scoring ---

// qualityResult is the parsed response from subagent quality scoring.
type qualityResult struct {
	Specificity float64  `json:"specificity"` // 0-1: how specific/actionable
	Uniqueness  float64  `json:"uniqueness"`  // 0-1: how novel vs existing info
	Entities    []string `json:"entities"`    // extracted named entities
	Confidence  float64  `json:"confidence"`  // 0-1: factual confidence
}

const qualitySystemPrompt = `You are a memory quality scorer. Given a memory summary (and optionally existing similar memories), evaluate it and return a JSON object:
{"specificity": 0.0-1.0, "uniqueness": 0.0-1.0, "entities": ["entity1", "entity2"], "confidence": 0.0-1.0}

- specificity: How specific and actionable is this information? (0=vague, 1=precise facts)
- uniqueness: How novel is this compared to existing similar memories shown below? (0=redundant/already known, 1=entirely new information)
- entities: Extract named entities (people, places, organizations, dates, products)
- confidence: How factually confident is this statement? (0=uncertain, 1=definitive)

Return ONLY the JSON object. No explanation.`

// qualityScoreResult holds the output from scoreQuality.
type qualityScoreResult struct {
	Entities   []string
	Confidence float64
	Salience   float64
}

// scoreQuality calls the subagent to evaluate memory quality and adjusts salience.
// When existingSummaries is non-empty, they are included for uniqueness comparison.
// Returns default values if subagentFn is nil or scoring fails (graceful degradation).
func scoreQuality(ctx context.Context, subagentFn SubagentFunc, summary string, baseSalience float64, existingSummaries []string) qualityScoreResult {
	result := qualityScoreResult{Confidence: 1.0, Salience: baseSalience}
	if subagentFn == nil {
		return result
	}

	// Build user content with optional existing memories for uniqueness comparison.
	var userContent string
	if len(existingSummaries) > 0 {
		var sb strings.Builder
		sb.WriteString("NEW MEMORY:\n")
		sb.WriteString(summary)
		sb.WriteString("\n\nEXISTING SIMILAR MEMORIES:\n")
		for i, s := range existingSummaries {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, s)
		}
		userContent = sb.String()
	} else {
		userContent = summary
	}

	scoreCtx, cancel := context.WithTimeout(ctx, TimeoutQualityScore)
	raw, err := subagentFn(scoreCtx, qualitySystemPrompt, userContent)
	cancel()
	if err != nil {
		logger.Log.Warnf("[memory] quality scoring failed, using defaults: %v", err)
		return result
	}

	var qr qualityResult
	if err := json.Unmarshal([]byte(raw), &qr); err != nil {
		logger.Log.Warnf("[memory] quality JSON parse failed: %v (raw: %s)", err, raw)
		return result
	}

	result.Entities = qr.Entities
	result.Confidence = qr.Confidence
	qualityBoost := (qr.Specificity + qr.Uniqueness) / 2.0
	result.Salience = baseSalience + (qualityBoost * (1.0 - baseSalience/10.0))
	if result.Salience > 10 {
		result.Salience = 10
	}
	return result
}

// ScoreAndWrite quality-scores a memory via the subagent, embeds it, and inserts it.
// Returns the new memory ID. If subagent scoring fails, the memory is still saved
// with default quality values (graceful degradation).
func ScoreAndWrite(ctx context.Context, db *pgxpool.Pool, req MemoryWriteRequest, subagentFn SubagentFunc) (int64, error) {
	qs := scoreQuality(ctx, subagentFn, req.Summary, req.Salience, nil)
	entities := qs.Entities
	confidence := qs.Confidence
	req.Salience = qs.Salience

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

// --- Entity extraction ---

const entityExtractionPrompt = `Extract named entities from this text. Return a JSON array of strings.
Include: people names, organizations, places, products, specific dates.
Exclude: generic nouns, pronouns, common words.
Return ONLY the JSON array. Example: ["John Smith", "Google", "Berlin"]
If no entities found, return: []`

// extractEntitiesLightweight uses the subagent to quickly extract named entities
// from a summary before contradiction search. Returns nil on failure (graceful degradation).
func extractEntitiesLightweight(ctx context.Context, subagentFn SubagentFunc, summary string) []string {
	if subagentFn == nil {
		return nil
	}

	extractCtx, cancel := context.WithTimeout(ctx, TimeoutQualityScore)
	raw, err := subagentFn(extractCtx, entityExtractionPrompt, summary)
	cancel()
	if err != nil {
		logger.Log.Warnf("[memory] entity extraction failed, skipping: %v", err)
		return nil
	}

	var entities []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &entities); err != nil {
		logger.Log.Warnf("[memory] entity extraction parse failed: %v (raw: %s)", err, raw)
		return nil
	}
	return entities
}

// --- Contradiction detection ---

const contradictionSystemPrompt = `You check whether a new memory contradicts existing memories. Given:
- NEW: the new memory
- EXISTING_1, EXISTING_2, etc.: existing memories to check

For each existing memory, output one line with the format:
EXISTING_N: CONTRADICTS
or
EXISTING_N: COMPATIBLE

A contradiction means the new memory directly updates, corrects, or overrides the existing one (e.g. changed preferences, updated facts).

Output ONLY the result lines, one per existing memory. No explanation.`

// contradictionCandidate holds a potential contradiction match.
type contradictionCandidate struct {
	ID      int64
	Summary string
}

// CheckAndWriteWithContradiction finds similar existing memories, checks for
// contradictions via the subagent, supersedes contradicted memories, and writes
// the new memory. Returns the new memory ID.
func CheckAndWriteWithContradiction(ctx context.Context, db *pgxpool.Pool, req MemoryWriteRequest, subagentFn SubagentFunc) (int64, error) {
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
	var candidates []contradictionCandidate
	var entities []string
	rows, err := db.Query(ctx,
		`SELECT id, summary FROM memories
		 WHERE superseded_by IS NULL
		   AND (embedding <=> $1) < 0.4
		 ORDER BY (embedding <=> $1)
		 LIMIT 3`,
		pgvector.NewVector(firstEmb),
	)
	if err != nil {
		logger.Log.Warnf("[memory] contradiction search failed: %v", err)
		// Still extract entities even if similarity search failed.
		entities = extractEntitiesLightweight(ctx, subagentFn, req.Summary)
	} else {
		for rows.Next() {
			var c contradictionCandidate
			if err := rows.Scan(&c.ID, &c.Summary); err != nil {
				continue
			}
			candidates = append(candidates, c)
		}
		rows.Close()

		// Run batched contradiction check and entity extraction concurrently.
		// They are independent subagent calls — parallelizing halves total wait time.
		var wg sync.WaitGroup

		if subagentFn != nil && len(candidates) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var promptBuilder strings.Builder
				fmt.Fprintf(&promptBuilder, "NEW: %s\n", req.Summary)
				for i, c := range candidates {
					fmt.Fprintf(&promptBuilder, "EXISTING_%d: %s\n", i+1, c.Summary)
				}

				checkCtx, cancel := context.WithTimeout(ctx, TimeoutContradictionCheck)
				raw, gErr := subagentFn(checkCtx, contradictionSystemPrompt, promptBuilder.String())
				cancel()
				if gErr != nil {
					logger.Log.Warnf("[memory] batched contradiction check failed: %v", gErr)
					return
				}
				for i, c := range candidates {
					tag := fmt.Sprintf("EXISTING_%d:", i+1)
					for _, line := range strings.Split(raw, "\n") {
						line = strings.TrimSpace(strings.ToUpper(line))
						if strings.HasPrefix(line, strings.ToUpper(tag)) && strings.Contains(line, "CONTRADICTS") {
							logger.Log.Infof("[memory] new memory contradicts id=%d, will supersede", c.ID)
							supersededIDs = append(supersededIDs, c.ID)
							break
						}
					}
				}
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			entities = extractEntitiesLightweight(ctx, subagentFn, req.Summary)
		}()

		wg.Wait()
	}

	// Entity-based contradiction lookup: find memories sharing entities but
	// not already found by embedding similarity. Uses the GIN index on entities.
	if len(entities) > 0 {
		existingIDs := make([]int64, len(candidates))
		for i, c := range candidates {
			existingIDs[i] = c.ID
		}
		entityRows, entityErr := db.Query(ctx,
			`SELECT id, summary FROM memories
			 WHERE superseded_by IS NULL
			   AND entities && $1
			   AND id != ALL($2)
			 ORDER BY created_at DESC
			 LIMIT 3`,
			entities, existingIDs,
		)
		if entityErr != nil {
			logger.Log.Warnf("[memory] entity-based contradiction search failed: %v", entityErr)
		} else {
			for entityRows.Next() {
				var c contradictionCandidate
				if err := entityRows.Scan(&c.ID, &c.Summary); err != nil {
					continue
				}
				candidates = append(candidates, c)
			}
			entityRows.Close()
		}
	}

	// Quality scoring via subagent (best-effort) — score once, apply to all chunks.
	// Pass candidate summaries for context-aware uniqueness scoring.
	var candidateSummaries []string
	if len(candidates) > 0 {
		candidateSummaries = make([]string, len(candidates))
		for i, c := range candidates {
			candidateSummaries[i] = c.Summary
		}
	}
	qs := scoreQuality(ctx, subagentFn, req.Summary, req.Salience, candidateSummaries)
	// Merge pre-extracted entities with quality-scored entities (dedup).
	scoredEntities := qs.Entities
	if len(scoredEntities) > 0 {
		seen := make(map[string]bool, len(entities))
		for _, e := range entities {
			seen[strings.ToLower(e)] = true
		}
		for _, e := range scoredEntities {
			if !seen[strings.ToLower(e)] {
				entities = append(entities, e)
				seen[strings.ToLower(e)] = true
			}
		}
	}
	confidence := qs.Confidence
	req.Salience = qs.Salience

	memType := req.MemoryType
	if memType == "" {
		memType = "general"
	}

	// Embed all chunks before starting the transaction (embedding is external I/O
	// and should not hold an open DB transaction).
	type preparedChunk struct {
		idx      int
		embedded []embeddedChunk
	}
	var prepared []preparedChunk
	for i, chunk := range chunks {
		var embs []embeddedChunk
		if i == 0 {
			embs = firstEmbedded
		} else {
			embs, err = embedWithFallback(ctx, req.EmbedEndpoint, req.EmbedModel, chunk)
			if err != nil {
				return 0, fmt.Errorf("embedding chunk %d failed: %w", i, err)
			}
		}
		prepared = append(prepared, preparedChunk{idx: i, embedded: embs})
	}

	// Insert all chunks + supersede in a single transaction.
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var firstID int64
	for _, pc := range prepared {
		for _, ec := range pc.embedded {
			var id int64
			err = tx.QueryRow(ctx,
				`INSERT INTO memories (summary, embedding, salience, tags, memory_type, entities, confidence, source, source_date)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 RETURNING id`,
				ec.Text, pgvector.NewVector(ec.Embedding), req.Salience, req.Tags,
				memType, entities, confidence, req.Source, req.SourceDate,
			).Scan(&id)
			if err != nil {
				return 0, fmt.Errorf("insert chunk %d failed: %w", pc.idx, err)
			}

			if firstID == 0 {
				firstID = id
			}
			logger.Log.Infof("[memory] contradiction-checked+saved id=%d chunk=%d/%d (salience=%.1f, entities=%v, source=%s): %s",
				id, pc.idx+1, len(chunks), req.Salience, entities, req.Source, ec.Text)
		}
	}

	// Supersede contradicted memories by pointing them at the first chunk's ID.
	for _, oldID := range supersededIDs {
		if _, err := tx.Exec(ctx,
			`UPDATE memories SET superseded_by = $1 WHERE id = $2`,
			firstID, oldID,
		); err != nil {
			return 0, fmt.Errorf("supersede id=%d: %w", oldID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}

	return firstID, nil
}
