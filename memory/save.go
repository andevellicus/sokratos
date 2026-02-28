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
	Retries      int // remaining retry attempts on transient failure (0 = no retry)
	OnComplete   func(result string, err error)
}

// WorkQueueFunc submits a background LLM task to the work queue. Items are
// processed sequentially as server slots become available. Nothing is dropped
// unless the queue buffer is completely full (64 items).
type WorkQueueFunc func(req WorkRequest)

// MemoryWriteRequest describes a memory to be quality-scored and written.
type MemoryWriteRequest struct {
	Summary       string
	Tags          []string
	Salience      float64
	MemoryType    string     // "general", "fact", "preference", "event", "email", "calendar"
	Source        string     // provenance: "conversation", "email", "calendar", "user", "backfill", "conversation_archive"
	SourceDate    *time.Time // original date of the source item (email sent date, event start, etc.)
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
	}, nil)
}

// SaveToMemoryWithSalienceAsync is like SaveToMemoryAsync but accepts a
// MemoryWriteRequest with custom salience, tags, and source. When grammarFn
// is non-nil, quality scoring (entities, confidence) is performed via ScoreAndWrite.
func SaveToMemoryWithSalienceAsync(db *pgxpool.Pool, req MemoryWriteRequest, grammarFn GrammarSubagentFunc) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutSaveOp)
		defer cancel()

		id, err := ScoreAndWrite(ctx, db, req, grammarFn)
		if err != nil {
			logger.Log.Errorf("[memory] async save failed: %v", err)
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
	return nil
}
