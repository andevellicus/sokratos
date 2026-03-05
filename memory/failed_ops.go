package memory

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/timeouts"
)

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
