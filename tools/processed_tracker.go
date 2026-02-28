package tools

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
// Fails open on query errors (includes the ID) to match the existing behavior
// in both email and calendar dedup paths.
func (pt *ProcessedTracker) FilterNew(ctx context.Context, ids []string) []string {
	var newIDs []string
	query := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE %s = $1)`, pt.Table, pt.IDColumn)
	for _, id := range ids {
		var exists bool
		err := pt.Pool.QueryRow(ctx, query, id).Scan(&exists)
		if err != nil {
			logger.Log.Warnf("[%s] dedup lookup failed for %s: %v", pt.Table, id, err)
			newIDs = append(newIDs, id)
			continue
		}
		if !exists {
			newIDs = append(newIDs, id)
		}
	}
	return newIDs
}

// MarkProcessed records the given IDs as processed (idempotent via ON CONFLICT).
func (pt *ProcessedTracker) MarkProcessed(ctx context.Context, ids []string) {
	query := fmt.Sprintf(`INSERT INTO %s (%s) VALUES ($1) ON CONFLICT DO NOTHING`, pt.Table, pt.IDColumn)
	for _, id := range ids {
		pt.Pool.Exec(ctx, query, id)
	}
}
