package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/textutil"
	"sokratos/tokens"
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
// and salience in the background without blocking the caller. When queueFn is
// non-nil, enrichment is submitted to the work queue (with retries) instead of
// a fire-and-forget goroutine that silently drops on busy.
func ScoreAndWrite(ctx context.Context, db *pgxpool.Pool, req MemoryWriteRequest, grammarFn GrammarSubagentFunc, queueFn WorkQueueFunc) (int64, error) {
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
			var pipelineID interface{}
			if req.PipelineID != 0 {
				pipelineID = req.PipelineID
			}
			err = db.QueryRow(ctx,
				`INSERT INTO memories (summary, embedding, salience, tags, memory_type, entities, confidence, source, source_date, pipeline_id)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				 RETURNING id`,
				ec.Text, pgvector.NewVector(ec.Embedding), req.Salience, req.Tags,
				memType, []string{}, 1.0, req.Source, req.SourceDate, pipelineID,
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

	if len(allIDs) > 0 {
		if queueFn != nil {
			submitEnrichment(queueFn, db, allIDs, req.Summary, req.Salience, nil)
		} else if grammarFn != nil {
			go EnrichViaGrammarFn(db, grammarFn, allIDs, req.Summary, req.Salience, nil)
		}
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
		LogFailedOp(db, "entity_enrichment", "quality_scoring", err, map[string]any{"memory_ids": ids})
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
		MaxTokens:    tokens.MemoryEnrichment,
		Timeout:      TimeoutQualityEnrich,
		Retries:      2,
		Priority:     PriorityNormal,
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

	// Quality gate: reject memories that are too vague or uncertain.
	if qr.Specificity < 0.2 || (qr.Specificity < 0.3 && qr.Confidence < 0.4) {
		delCtx, delCancel := context.WithTimeout(context.Background(), TimeoutSaveOp)
		defer delCancel()
		_, err := db.Exec(delCtx, `DELETE FROM memories WHERE id = ANY($1)`, ids)
		if err != nil {
			logger.Log.Warnf("[memory] quality gate delete failed: %v", err)
		} else {
			logger.Log.Infof("[memory] quality gate rejected ids=%v (specificity=%.2f, confidence=%.2f)",
				ids, qr.Specificity, qr.Confidence)
		}
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
