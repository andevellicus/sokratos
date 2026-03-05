package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/timeouts"
)

// SubagentFunc calls a subagent model with system + user prompts and
// returns the response text. The caller creates this as a closure over the
// actual LLM client, keeping the memory package free of llm/tools imports.
type SubagentFunc func(ctx context.Context, systemPrompt, userPrompt string) (string, error)

// GrammarSubagentFunc is like SubagentFunc but accepts a GBNF grammar string
// to constrain the model's output. Used for quality scoring and entity
// extraction where Flash models ignore "return ONLY JSON" instructions and
// dump chain-of-thought reasoning unless grammar-constrained.
type GrammarSubagentFunc func(ctx context.Context, systemPrompt, userPrompt, grammar string) (string, error)

// WorkPriority controls admission under queue pressure. Higher values have
// higher priority. Zero is treated as PriorityNormal for backward compat.
type WorkPriority int

const (
	PriorityLow    WorkPriority = 1 // background enrichment — droppable under pressure
	PriorityNormal WorkPriority = 2 // retryable correctness work (quality scoring, contradiction)
	PriorityHigh   WorkPriority = 3 // critical work (distillation — data lost if dropped)
)

// WorkRequest describes a background LLM task to be processed by the work
// queue. Each item gets its own fresh context (with Timeout duration), so
// queue wait time does not eat into inference time.
type WorkRequest struct {
	Label        string
	SystemPrompt string
	UserPrompt   string
	Grammar      string
	MaxTokens    int
	Timeout      time.Duration
	Retries      int          // remaining retry attempts on transient failure (0 = no retry)
	Priority     WorkPriority // 0 treated as PriorityNormal
	OnComplete   func(result string, err error)
}

// WorkQueueFunc submits a background LLM task to the work queue. Items are
// processed as worker goroutines become available. Low-priority items are
// dropped (and deferred for retry) when the queue is ≥75% full; all items
// are dropped when the buffer is completely full.
type WorkQueueFunc func(req WorkRequest)

// MemoryWriteRequest describes a memory to be quality-scored and written.
type MemoryWriteRequest struct {
	Summary       string
	Tags          []string
	Salience      float64
	MemoryType    string     // "general", "fact", "preference", "event", "email", "calendar"
	Source        string     // provenance: "conversation", "email", "calendar", "user", "backfill", "conversation_archive"
	SourceDate    *time.Time // original date of the source item (email sent date, event start, etc.)
	PipelineID    int64      // Telegram message ID of the originating pipeline; 0 = no tracking
	EmbedEndpoint string
	EmbedModel    string
}

// MaxChunkBytes is the approximate byte limit per embedding chunk.
// BGE-large-en-v1.5 has a 512-token context. WordPiece tokenization
// ranges from ~4 bytes/token (plain English) down to ~2.8 bytes/token
// for structured/HTML content. 1200 bytes stays safely under the
// 512-token hard limit even for dense email content (~425 tokens at
// worst case).
const MaxChunkBytes = 1200

// MaxSupersededProfiles is the maximum number of identity profile rows
// to retain in the memories table. When a new profile is written and the
// total exceeds this limit, the oldest superseded profiles are purged.
// Set via MAX_SUPERSEDED_PROFILES env var (default 20).
var MaxSupersededProfiles = 20

// SaveToMemoryAsync embeds content and inserts it into the memories table
// on a background goroutine so it doesn't block the caller. Content that
// exceeds the embedding model's context window is split into chunks.
func SaveToMemoryAsync(db *pgxpool.Pool, embedEndpoint, embedModel, tag, content string) {
	SaveToMemoryWithSalienceAsync(db, MemoryWriteRequest{
		Summary:       content,
		Tags:          []string{tag},
		Salience:      3.0,
		Source:        "conversation_archive",
		EmbedEndpoint: embedEndpoint,
		EmbedModel:    embedModel,
	}, nil, nil)
}

// SaveToMemoryWithSalienceAsync is like SaveToMemoryAsync but accepts a
// MemoryWriteRequest with custom salience, tags, and source. When queueFn is
// non-nil, enrichment is submitted to the work queue (preferred). Otherwise,
// when grammarFn is non-nil, enrichment runs as a fire-and-forget goroutine.
func SaveToMemoryWithSalienceAsync(db *pgxpool.Pool, req MemoryWriteRequest, grammarFn GrammarSubagentFunc, queueFn WorkQueueFunc) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutSaveOp)
		defer cancel()

		id, err := ScoreAndWrite(ctx, db, req, grammarFn, queueFn)
		if err != nil {
			logger.Log.Errorf("[memory] async save failed: %v", err)
			LogFailedOp(db, "memory_save", req.Source, err, map[string]any{"tags": req.Tags})
			return
		}
		logger.Log.Infof("[memory] saved id=%d (salience=%.0f, tags=%v, source=%s)", id, req.Salience, req.Tags, req.Source)
	}()
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

// ChunkText splits text into pieces of at most maxBytes, breaking at the
// last newline before the limit to avoid cutting mid-sentence.
// minChunkBytes is the minimum useful chunk size. Fragments below this
// threshold are discarded — they're typically truncated tail-end remnants
// ("Stat...", "I've added Mary's dinne...") that waste an embedding slot
// and pollute search results.
const minChunkBytes = 50

func ChunkText(text string, maxBytes int) []string {
	if len(text) <= maxBytes {
		return []string{text}
	}

	original := text
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

		chunk := strings.TrimSpace(text[:end])
		if len(chunk) >= minChunkBytes {
			chunks = append(chunks, chunk)
		}
		text = text[end:]
	}
	// If everything was discarded (shouldn't happen), keep the original.
	if len(chunks) == 0 {
		return []string{strings.TrimSpace(original)}
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
		if errors.Is(err, pgx.ErrNoRows) {
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
	if len(embedded) == 0 {
		return fmt.Errorf("embedding returned no vectors for identity profile")
	}

	// Insert + supersede in a single transaction.
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var newID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO memories (summary, embedding, salience, tags, memory_type, source, entities)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id`,
		profileJSON, pgvector.NewVector(embedded[0].Embedding), 10.0, []string{"identity", "profile"},
		"identity", "consolidation", []string{},
	).Scan(&newID)
	if err != nil {
		return fmt.Errorf("insert identity profile: %w", err)
	}

	// Supersede all previous identity rows.
	_, err = tx.Exec(ctx,
		`UPDATE memories SET superseded_by = $1 WHERE memory_type = 'identity' AND superseded_by IS NULL AND id != $1`,
		newID,
	)
	if err != nil {
		return fmt.Errorf("supersede old identity profiles: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	logger.Log.Infof("[memory] identity profile written (id=%d, %d bytes)", newID, len(profileJSON))

	// Purge oldest superseded profiles if we exceed the retention limit.
	if MaxSupersededProfiles > 0 {
		go purgeSupersededProfiles(db, MaxSupersededProfiles)
	}
	return nil
}

// purgeSupersededProfiles deletes identity profile rows beyond the retention
// limit, keeping the most recent `keep` rows (the active profile is always
// the newest and is therefore always retained).
func purgeSupersededProfiles(db *pgxpool.Pool, keep int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
	defer cancel()

	res, err := db.Exec(ctx,
		`DELETE FROM memories
		 WHERE memory_type = 'identity'
		   AND id NOT IN (
		       SELECT id FROM memories
		       WHERE memory_type = 'identity'
		       ORDER BY created_at DESC
		       LIMIT $1
		   )`,
		keep,
	)
	if err != nil {
		logger.Log.Warnf("[memory] failed to purge superseded profiles: %v", err)
		return
	}
	if n := res.RowsAffected(); n > 0 {
		logger.Log.Infof("[memory] purged %d superseded identity profiles (kept %d)", n, keep)
	}
}
