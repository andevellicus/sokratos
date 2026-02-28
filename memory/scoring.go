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
	"sokratos/textutil"
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

// qualityGrammar is a GBNF grammar constraining quality scoring output to a
// valid JSON object with the expected fields. Without this, Flash models dump
// chain-of-thought reasoning instead of JSON.
const qualityGrammar = `root ::= "{" ws "\"specificity\":" ws number "," ws "\"uniqueness\":" ws number "," ws "\"entities\":" ws array "," ws "\"confidence\":" ws number ws "}"
number ::= [0-9] ("." [0-9]+)?
array ::= "[]" | "[" ws string (ws "," ws string)* ws "]"
string ::= "\"" [^"\\]* "\""
ws ::= [ \t\n\r]*`

// entityGrammar is a GBNF grammar constraining entity extraction output to a
// JSON array of strings.
const entityGrammar = `root ::= "[]" | "[" ws string (ws "," ws string)* ws "]"
string ::= "\"" [^"\\]* "\""
ws ::= [ \t\n\r]*`

// buildEnrichmentPrompt formats the user content for quality scoring,
// optionally including existing memories for uniqueness comparison.
func buildEnrichmentPrompt(summary string, existingSummaries []string) string {
	if len(existingSummaries) == 0 {
		return summary
	}
	var sb strings.Builder
	sb.WriteString("NEW MEMORY:\n")
	sb.WriteString(summary)
	sb.WriteString("\n\nEXISTING SIMILAR MEMORIES:\n")
	for i, s := range existingSummaries {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, s)
	}
	return sb.String()
}

// ScoreAndWrite embeds and inserts a memory with default quality values, then
// fires async quality enrichment via the subagent (if available). The memory is
// immediately available for retrieval; enrichment updates entities, confidence,
// and salience in the background without blocking the caller.
func ScoreAndWrite(ctx context.Context, db *pgxpool.Pool, req MemoryWriteRequest, grammarFn GrammarSubagentFunc) (int64, error) {
	memType := req.MemoryType
	if memType == "" {
		memType = "general"
	}

	chunks := ChunkText(req.Summary, MaxChunkBytes)
	var firstID int64
	var allIDs []int64

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
				memType, []string{}, 1.0, req.Source, req.SourceDate,
			).Scan(&id)
			if err != nil {
				return firstID, fmt.Errorf("insert chunk %d failed: %w", i, err)
			}

			if firstID == 0 {
				firstID = id
			}
			allIDs = append(allIDs, id)
			logger.Log.Infof("[memory] saved id=%d chunk=%d/%d (salience=%.1f, source=%s): %s",
				id, i+1, len(chunks), req.Salience, req.Source, ec.Text)
		}
	}

	if grammarFn != nil && len(allIDs) > 0 {
		go EnrichViaGrammarFn(db, grammarFn, allIDs, req.Summary, req.Salience, nil)
	}

	return firstID, nil
}

// EnrichViaGrammarFn runs quality scoring synchronously via grammarFn and
// applies the result. Used by ScoreAndWrite where no work queue is available,
// and by episode/reflection/consolidation for post-insert entity extraction.
func EnrichViaGrammarFn(db *pgxpool.Pool, grammarFn GrammarSubagentFunc, ids []int64, summary string, baseSalience float64, existingSummaries []string) {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutQualityEnrich)
	defer cancel()

	userContent := buildEnrichmentPrompt(summary, existingSummaries)
	raw, err := grammarFn(ctx, qualitySystemPrompt, userContent, qualityGrammar)
	if err != nil {
		logger.Log.Warnf("[memory] quality scoring failed: %v", err)
		return
	}
	applyEnrichment(db, ids, raw, baseSalience)
}

// submitEnrichment queues a quality enrichment task via the work queue.
// The LLM call runs in the queue worker with its own timeout; the callback
// parses the result and updates the DB.
func submitEnrichment(queueFn WorkQueueFunc, db *pgxpool.Pool, ids []int64, summary string, baseSalience float64, existingSummaries []string) {
	queueFn(WorkRequest{
		Label:        "quality-enrichment",
		SystemPrompt: qualitySystemPrompt,
		UserPrompt:   buildEnrichmentPrompt(summary, existingSummaries),
		Grammar:      qualityGrammar,
		MaxTokens:    512,
		Timeout:      TimeoutQualityEnrich,
		Retries:      2,
		OnComplete: func(raw string, err error) {
			if err != nil {
				logger.Log.Warnf("[memory] quality enrichment failed: %v", err)
				return
			}
			applyEnrichment(db, ids, raw, baseSalience)
		},
	})
}

// applyEnrichment parses quality scoring output and updates memory rows.
func applyEnrichment(db *pgxpool.Pool, ids []int64, raw string, baseSalience float64) {
	cleaned := textutil.CleanLLMJSON(raw)
	var qr qualityResult
	if err := json.Unmarshal([]byte(cleaned), &qr); err != nil {
		logger.Log.Warnf("[memory] quality JSON parse failed: %v (raw: %s)", err, raw)
		return
	}

	qualityBoost := (qr.Specificity + qr.Uniqueness) / 2.0
	salience := baseSalience + (qualityBoost * (1.0 - baseSalience/10.0))
	if salience > 10 {
		salience = 10
	}

	ctx, cancel := context.WithTimeout(context.Background(), TimeoutSaveOp)
	defer cancel()

	_, err := db.Exec(ctx,
		`UPDATE memories
		 SET entities = (SELECT ARRAY(SELECT DISTINCT e FROM unnest(entities || $1::text[]) AS e)),
		     confidence = $2,
		     salience = $3
		 WHERE id = ANY($4)`,
		qr.Entities, qr.Confidence, salience, ids,
	)
	if err != nil {
		logger.Log.Warnf("[memory] async quality enrichment failed: %v", err)
		return
	}
	logger.Log.Infof("[memory] enriched ids=%v (salience=%.1f→%.1f, entities=%v, confidence=%.2f)",
		ids, baseSalience, salience, qr.Entities, qr.Confidence)
}

// --- Entity extraction ---

const entityExtractionPrompt = `Extract named entities from this text. Return a JSON array of strings.
Include: people names, organizations, places, products, specific dates.
Exclude: generic nouns, pronouns, common words.
Return ONLY the JSON array. Example: ["John Smith", "Google", "Berlin"]
If no entities found, return: []`

// extractEntitiesLightweight uses the subagent (with GBNF grammar) to quickly
// extract named entities from a summary before contradiction search. Returns
// nil on failure (graceful degradation).
func extractEntitiesLightweight(ctx context.Context, grammarFn GrammarSubagentFunc, summary string) []string {
	if grammarFn == nil {
		return nil
	}

	extractCtx, cancel := context.WithTimeout(context.Background(), TimeoutQualityScore)
	raw, err := grammarFn(extractCtx, entityExtractionPrompt, summary, entityGrammar)
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
// contradictions, supersedes contradicted memories, and writes the new memory.
// Returns the new memory ID.
//
// Function parameters serve distinct roles:
//   - bgGrammarFn: non-blocking (TryComplete), for contradiction detection and entity extraction.
//     When backends are busy, the memory is saved immediately and deferred work is queued.
//   - queueFn: fire-and-forget work queue for background quality enrichment and deferred work
func CheckAndWriteWithContradiction(ctx context.Context, db *pgxpool.Pool, req MemoryWriteRequest, bgGrammarFn GrammarSubagentFunc, queueFn WorkQueueFunc) (int64, error) {
	// Chunk first; use first chunk for contradiction search embedding.
	chunks := ChunkText(req.Summary, MaxChunkBytes)

	// Embed the first chunk with fallback — needed for both similarity search and insert.
	firstEmbedded, err := embedWithFallback(ctx, req.EmbedEndpoint, req.EmbedModel, chunks[0])
	if err != nil {
		return 0, fmt.Errorf("embedding failed: %w", err)
	}
	if len(firstEmbedded) == 0 {
		return 0, fmt.Errorf("embedding returned no vectors")
	}
	// Use the first sub-chunk's embedding for contradiction search.
	firstEmb := firstEmbedded[0].Embedding

	// Find top 3 most similar non-superseded memories.
	var supersededIDs []int64
	var candidates []contradictionCandidate
	var entities []string
	var contradictionDeferred, entityDeferred bool
	rows, err := db.Query(ctx,
		`SELECT id, summary FROM memories
		 WHERE superseded_by IS NULL
		   AND memory_type != 'identity'
		   AND (embedding <=> $1) < 0.4
		 ORDER BY (embedding <=> $1)
		 LIMIT 3`,
		pgvector.NewVector(firstEmb),
	)
	if err != nil {
		logger.Log.Warnf("[memory] contradiction search failed: %v", err)
		// Still extract entities even if similarity search failed.
		entities = extractEntitiesLightweight(ctx, bgGrammarFn, req.Summary)
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
		// Both use bgGrammarFn (non-blocking TryComplete) — if backends are
		// busy, the memory is saved immediately and deferred work is queued.
		var wg sync.WaitGroup

		if bgGrammarFn != nil && len(candidates) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Use only the summary portion for contradiction comparison.
				// The "Source exchange:" section is appended for archival, not
				// for semantic comparison — it can cause false contradictions
				// when the raw exchange mentions other people's information.
				summaryForComparison := req.Summary
				if idx := strings.Index(summaryForComparison, "\n\nSource exchange:\n"); idx >= 0 {
					summaryForComparison = summaryForComparison[:idx]
				}
				var promptBuilder strings.Builder
				fmt.Fprintf(&promptBuilder, "NEW: %s\n", summaryForComparison)
				for i, c := range candidates {
					fmt.Fprintf(&promptBuilder, "EXISTING_%d: %s\n", i+1, c.Summary)
				}

				checkCtx, cancel := context.WithTimeout(context.Background(), TimeoutContradictionCheck)
				raw, gErr := bgGrammarFn(checkCtx, contradictionSystemPrompt, promptBuilder.String(), "")
				cancel()
				if gErr != nil {
					logger.Log.Warnf("[memory] contradiction check deferred (busy): %v", gErr)
					contradictionDeferred = true
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

		if bgGrammarFn != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				entities = extractEntitiesLightweight(ctx, bgGrammarFn, req.Summary)
				if entities == nil {
					entityDeferred = true
				}
			}()
		}

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
			   AND memory_type != 'identity'
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

	// Collect candidate summaries for async quality enrichment (context-aware
	// uniqueness scoring). Quality scoring is deferred to avoid blocking the save.
	var candidateSummaries []string
	if len(candidates) > 0 {
		candidateSummaries = make([]string, len(candidates))
		for i, c := range candidates {
			candidateSummaries[i] = c.Summary
		}
	}

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
	var allIDs []int64
	for _, pc := range prepared {
		for _, ec := range pc.embedded {
			var id int64
			err = tx.QueryRow(ctx,
				`INSERT INTO memories (summary, embedding, salience, tags, memory_type, entities, confidence, source, source_date)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 RETURNING id`,
				ec.Text, pgvector.NewVector(ec.Embedding), req.Salience, req.Tags,
				memType, entities, 1.0, req.Source, req.SourceDate,
			).Scan(&id)
			if err != nil {
				return 0, fmt.Errorf("insert chunk %d failed: %w", pc.idx, err)
			}

			if firstID == 0 {
				firstID = id
			}
			allIDs = append(allIDs, id)
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

	// Queue deferred work that was skipped because backends were busy.
	// These run asynchronously via the work queue and retroactively update
	// the already-committed memory rows.
	if contradictionDeferred && queueFn != nil && len(candidates) > 0 {
		submitDeferredContradiction(queueFn, db, firstID, candidates, req.Summary)
	}
	if entityDeferred && queueFn != nil && len(allIDs) > 0 {
		submitDeferredEntityExtraction(queueFn, db, allIDs, req.Summary)
	}

	// Fire async quality enrichment after successful commit via the work
	// queue. Each queued item gets its own fresh context, so enrichment
	// never competes for the caller's timeout budget.
	if queueFn != nil && len(allIDs) > 0 {
		submitEnrichment(queueFn, db, allIDs, req.Summary, req.Salience, candidateSummaries)
	}

	return firstID, nil
}

// submitDeferredContradiction queues a contradiction check that was skipped
// because the backend was busy. On completion, supersedes contradicted memories
// retroactively via UPDATE.
func submitDeferredContradiction(queueFn WorkQueueFunc, db *pgxpool.Pool, newID int64, candidates []contradictionCandidate, summary string) {
	summaryForComparison := summary
	if idx := strings.Index(summaryForComparison, "\n\nSource exchange:\n"); idx >= 0 {
		summaryForComparison = summaryForComparison[:idx]
	}

	var promptBuilder strings.Builder
	fmt.Fprintf(&promptBuilder, "NEW: %s\n", summaryForComparison)
	for i, c := range candidates {
		fmt.Fprintf(&promptBuilder, "EXISTING_%d: %s\n", i+1, c.Summary)
	}

	// Capture candidates slice for the closure.
	capturedCandidates := make([]contradictionCandidate, len(candidates))
	copy(capturedCandidates, candidates)

	queueFn(WorkRequest{
		Label:        "deferred-contradiction",
		SystemPrompt: contradictionSystemPrompt,
		UserPrompt:   promptBuilder.String(),
		Grammar:      "", // plain text output (no grammar constraint)
		MaxTokens:    512,
		Timeout:      TimeoutContradictionCheck,
		Retries:      2,
		OnComplete: func(raw string, err error) {
			if err != nil {
				logger.Log.Warnf("[memory] deferred contradiction check failed: %v", err)
				LogFailedOp(db, "contradiction_check", fmt.Sprintf("deferred for id=%d", newID), err, map[string]any{
					"new_id":     newID,
					"candidates": len(capturedCandidates),
				})
				return
			}
			for i, c := range capturedCandidates {
				tag := fmt.Sprintf("EXISTING_%d:", i+1)
				for _, line := range strings.Split(raw, "\n") {
					line = strings.TrimSpace(strings.ToUpper(line))
					if strings.HasPrefix(line, strings.ToUpper(tag)) && strings.Contains(line, "CONTRADICTS") {
						ctx, cancel := context.WithTimeout(context.Background(), TimeoutSaveOp)
						_, dbErr := db.Exec(ctx,
							`UPDATE memories SET superseded_by = $1 WHERE id = $2 AND superseded_by IS NULL`,
							newID, c.ID)
						cancel()
						if dbErr != nil {
							logger.Log.Warnf("[memory] deferred supersede id=%d failed: %v", c.ID, dbErr)
						} else {
							logger.Log.Infof("[memory] deferred supersede: id=%d superseded by id=%d", c.ID, newID)
						}
						break
					}
				}
			}
		},
	})
	logger.Log.Infof("[memory] queued deferred contradiction check for id=%d (%d candidates)", newID, len(candidates))
}

// submitDeferredEntityExtraction queues entity extraction that was skipped
// because the backend was busy. On completion, merges extracted entities into
// the already-committed memory rows via UPDATE.
func submitDeferredEntityExtraction(queueFn WorkQueueFunc, db *pgxpool.Pool, ids []int64, summary string) {
	// Capture IDs slice for the closure.
	capturedIDs := make([]int64, len(ids))
	copy(capturedIDs, ids)

	queueFn(WorkRequest{
		Label:        "deferred-entity-extraction",
		SystemPrompt: entityExtractionPrompt,
		UserPrompt:   summary,
		Grammar:      entityGrammar,
		MaxTokens:    512,
		Timeout:      TimeoutQualityScore,
		Retries:      2,
		OnComplete: func(raw string, err error) {
			if err != nil {
				logger.Log.Warnf("[memory] deferred entity extraction failed: %v", err)
				LogFailedOp(db, "entity_extraction", fmt.Sprintf("deferred for ids=%v", capturedIDs), err, map[string]any{
					"memory_ids": capturedIDs,
				})
				return
			}
			var extracted []string
			if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(raw)), &extracted); jsonErr != nil {
				logger.Log.Warnf("[memory] deferred entity extraction parse failed: %v (raw: %s)", jsonErr, raw)
				return
			}
			if len(extracted) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), TimeoutSaveOp)
			_, dbErr := db.Exec(ctx,
				`UPDATE memories SET entities = (SELECT ARRAY(SELECT DISTINCT e FROM unnest(entities || $1::text[]) AS e)) WHERE id = ANY($2)`,
				extracted, capturedIDs)
			cancel()
			if dbErr != nil {
				logger.Log.Warnf("[memory] deferred entity merge failed: %v", dbErr)
			} else {
				logger.Log.Infof("[memory] deferred entities applied to ids=%v: %v", capturedIDs, extracted)
			}
		},
	})
	logger.Log.Infof("[memory] queued deferred entity extraction for ids=%v", ids)
}
