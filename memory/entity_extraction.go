package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/tokens"
)

// --- Entity extraction ---

var entityExtractionPrompt = strings.TrimSpace(prompts.EntityExtraction)

// entityGrammar is a GBNF grammar constraining entity extraction output to a
// JSON array of strings.
const entityGrammar = `root ::= "[]" | "[" ws string (ws "," ws string)* ws "]"
string ::= "\"" [^"\\]* "\""
ws ::= [ \t\n\r]*`

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
		MaxTokens:    tokens.MemoryEnrichment,
		Timeout:      TimeoutQualityScore,
		Retries:      2,
		Priority:     PriorityLow,
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
