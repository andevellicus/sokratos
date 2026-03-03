package pipelines

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
)

// ProcessedTracker provides deduplication against a "processed items" table.
// Both processed_emails (message_id) and processed_events (event_uid) follow
// the same pattern: check existence, filter new, mark processed.
type ProcessedTracker struct {
	Pool     *pgxpool.Pool
	Table    string // "processed_emails" or "processed_events"
	IDColumn string // "message_id" or "event_uid"
}

// FilterNew returns only the IDs that are not yet recorded in the tracker table.
// Uses a single ANY($1) query instead of one query per ID. Fails open on query
// errors (returns all IDs) to match the existing fail-open behavior.
func (pt *ProcessedTracker) FilterNew(ctx context.Context, ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ANY($1)`, pt.IDColumn, pt.Table, pt.IDColumn)
	rows, err := pt.Pool.Query(ctx, query, ids)
	if err != nil {
		logger.Log.Warnf("[%s] dedup batch lookup failed: %v", pt.Table, err)
		return ids // fail-open
	}
	defer rows.Close()

	existing := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			existing[id] = struct{}{}
		}
	}

	newIDs := ids[:0:0] // reuse backing array hint, zero-length
	for _, id := range ids {
		if _, seen := existing[id]; !seen {
			newIDs = append(newIDs, id)
		}
	}
	return newIDs
}

// MarkProcessed records the given IDs as processed (idempotent via ON CONFLICT).
// Uses a single batch INSERT with unnest instead of one statement per ID.
func (pt *ProcessedTracker) MarkProcessed(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	query := fmt.Sprintf(
		`INSERT INTO %s (%s) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`,
		pt.Table, pt.IDColumn,
	)
	if _, err := pt.Pool.Exec(ctx, query, ids); err != nil {
		logger.Log.Warnf("[%s] batch mark-processed failed: %v", pt.Table, err)
	}
}
