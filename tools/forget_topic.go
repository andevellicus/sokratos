package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/textutil"
)

type forgetTopicArgs struct {
	Topic   string `json:"topic"`
	Confirm bool   `json:"confirm"`
}

// NewForgetTopic returns a ToolFunc that archives all memories related to a
// topic. In preview mode (confirm=false), it returns a count and first 5
// summaries. In archive mode (confirm=true), it bulk-supersedes matching
// memories and creates an archive note.
func NewForgetTopic(pool *pgxpool.Pool, embedEndpoint, embedModel string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a forgetTopicArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}

		topic := strings.TrimSpace(a.Topic)
		if topic == "" {
			return "error: topic must not be empty", nil
		}

		ctx, cancel := context.WithTimeout(ctx, TimeoutForgetTopic)
		defer cancel()

		// Embed the topic for vector similarity search.
		emb, err := memory.GetEmbedding(ctx, embedEndpoint, embedModel, topic)
		if err != nil {
			return "", fmt.Errorf("embed topic: %w", err)
		}

		// Query matching non-superseded memories.
		rows, err := pool.Query(ctx,
			`SELECT id, summary FROM memories
			 WHERE superseded_by IS NULL
			   AND (embedding <=> $1) < 0.35
			 ORDER BY (embedding <=> $1)
			 LIMIT 50`,
			pgvector.NewVector(emb),
		)
		if err != nil {
			return "", fmt.Errorf("query matching memories: %w", err)
		}
		defer rows.Close()

		type match struct {
			ID      int64
			Summary string
		}
		var matches []match
		for rows.Next() {
			var m match
			if err := rows.Scan(&m.ID, &m.Summary); err != nil {
				continue
			}
			matches = append(matches, m)
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("iterate matching memories: %w", err)
		}

		if len(matches) == 0 {
			return fmt.Sprintf("No memories found matching topic %q.", topic), nil
		}

		// Preview mode: show count + first 5 summaries.
		if !a.Confirm {
			var b strings.Builder
			fmt.Fprintf(&b, "Found %d memories matching %q:\n", len(matches), topic)
			limit := 5
			if limit > len(matches) {
				limit = len(matches)
			}
			for i := 0; i < limit; i++ {
				fmt.Fprintf(&b, "- %s\n", textutil.Truncate(matches[i].Summary, 100))
			}
			if len(matches) > 5 {
				fmt.Fprintf(&b, "... and %d more.\n", len(matches)-5)
			}
			b.WriteString("\nCall again with confirm=true to archive these memories.")
			return b.String(), nil
		}

		// Archive mode: create archive memory + bulk-supersede in a transaction.
		ids := make([]int64, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}

		archiveSummary := fmt.Sprintf("Topic archived: %s. %d memories superseded.", topic, len(matches))

		tx, err := pool.Begin(ctx)
		if err != nil {
			return "", fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback(ctx)

		// Insert archive memory.
		archiveEmb, err := memory.GetEmbedding(ctx, embedEndpoint, embedModel, archiveSummary)
		if err != nil {
			return "", fmt.Errorf("embed archive summary: %w", err)
		}

		var archiveID int64
		err = tx.QueryRow(ctx,
			`INSERT INTO memories (summary, embedding, salience, tags, memory_type, source)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 RETURNING id`,
			archiveSummary, pgvector.NewVector(archiveEmb), 3.0,
			[]string{"archive", "forgotten"}, "archive", "forget_topic",
		).Scan(&archiveID)
		if err != nil {
			return "", fmt.Errorf("insert archive memory: %w", err)
		}

		// Bulk-supersede matching memories.
		_, err = tx.Exec(ctx,
			`UPDATE memories SET superseded_by = $1 WHERE id = ANY($2)`,
			archiveID, ids,
		)
		if err != nil {
			return "", fmt.Errorf("supersede memories: %w", err)
		}

		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit transaction: %w", err)
		}

		logger.Log.Infof("[forget_topic] archived %d memories for topic %q (archive_id=%d)", len(matches), topic, archiveID)
		return fmt.Sprintf("Archived %d memories related to %q. Archive memory created (id=%d).", len(matches), topic, archiveID), nil
	}
}
