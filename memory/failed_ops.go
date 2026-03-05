package memory

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/timeouts"
)

// FailureSummary groups recent failures by operation type.
type FailureSummary struct {
	OpType    string
	Count     int
	LastError string
	LastSeen  time.Time
}

// QueryRecentFailures returns aggregated failure summaries within the given
// time window, ordered by most recent first. Returns nil on any error
// (fail-open so heartbeat continues).
func QueryRecentFailures(ctx context.Context, db *pgxpool.Pool, window time.Duration, limit int) []FailureSummary {
	rows, err := db.Query(ctx,
		`SELECT op_type, COUNT(*)::int, (array_agg(error_message ORDER BY created_at DESC))[1], MAX(created_at)
		 FROM failed_operations
		 WHERE created_at >= NOW() - $1::interval
		 GROUP BY op_type
		 ORDER BY MAX(created_at) DESC
		 LIMIT $2`,
		window, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var results []FailureSummary
	for rows.Next() {
		var fs FailureSummary
		if err := rows.Scan(&fs.OpType, &fs.Count, &fs.LastError, &fs.LastSeen); err != nil {
			continue
		}
		results = append(results, fs)
	}
	return results
}

// LogFailedOp records a failed cognitive operation to the failed_operations
// table. Fire-and-forget: uses a 5s timeout and never propagates errors.
func LogFailedOp(pool *pgxpool.Pool, opType, label string, opErr error, contextData map[string]any) {
	if pool == nil {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
		defer cancel()

		var ctxJSON []byte
		if contextData != nil {
			ctxJSON, _ = json.Marshal(contextData)
		}

		_, dbErr := pool.Exec(ctx,
			`INSERT INTO failed_operations (op_type, label, error_message, context_data)
			 VALUES ($1, $2, $3, $4)`,
			opType, label, opErr.Error(), ctxJSON,
		)
		if dbErr != nil {
			logger.Log.Warnf("[failed_ops] could not log %s/%s: %v", opType, label, dbErr)
		}
	}()
}
