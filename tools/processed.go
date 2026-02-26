package tools

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ProcessedTable identifies which dedup table and column to query.
type ProcessedTable struct {
	Table  string // "processed_emails" or "processed_events"
	Column string // "gmail_id" or "event_id"
}

var (
	ProcessedEmails = ProcessedTable{"processed_emails", "gmail_id"}
	ProcessedEvents = ProcessedTable{"processed_events", "event_id"}
)

// LookupProcessedIDs returns the subset of ids that already exist in the given
// processed table. Used for dedup in both check and backfill pipelines.
func LookupProcessedIDs(ctx context.Context, pool *pgxpool.Pool, pt ProcessedTable, ids []string) (map[string]struct{}, error) {
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ANY($1)", pt.Column, pt.Table, pt.Column)
	rows, err := pool.Query(ctx, query, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		seen[id] = struct{}{}
	}
	return seen, rows.Err()
}

// FilterProcessed returns only items whose IDs are not already in the given
// processed table. The getID function extracts the dedup key from each item.
func FilterProcessed[T any](ctx context.Context, pool *pgxpool.Pool, table ProcessedTable, items []T, getID func(T) string) ([]T, error) {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = getID(item)
	}

	seen, err := LookupProcessedIDs(ctx, pool, table, ids)
	if err != nil {
		return items, err
	}

	var fresh []T
	for _, item := range items {
		if _, ok := seen[getID(item)]; !ok {
			fresh = append(fresh, item)
		}
	}
	return fresh, nil
}

// RecordProcessedID inserts a single ID into the given processed table.
func RecordProcessedID(ctx context.Context, pool *pgxpool.Pool, pt ProcessedTable, id string) error {
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES ($1) ON CONFLICT DO NOTHING", pt.Table, pt.Column)
	_, err := pool.Exec(ctx, query, id)
	return err
}
