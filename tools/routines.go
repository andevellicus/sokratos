package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type routineArgs struct {
	Action      string `json:"action"`                // "upsert" or "delete"
	Name        string `json:"name"`                  // unique routine name
	Interval    string `json:"interval,omitempty"`    // Go duration, e.g. "24h", "1h"
	Instruction string `json:"instruction,omitempty"` // what to do when the routine fires
}

// NewManageRoutines returns a ToolFunc that creates, updates, or deletes
// autonomous routine items in the PostgreSQL routines table.
func NewManageRoutines(pool *pgxpool.Pool) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a routineArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Name == "" {
			return "error: name is required", nil
		}

		switch a.Action {
		case "upsert":
			if a.Instruction == "" {
				return "error: instruction is required for upsert", nil
			}
			if a.Interval == "" {
				return "error: interval is required for upsert", nil
			}

			// Parse the Go duration string to validate it, then convert to
			// a PostgreSQL-compatible interval literal ("N seconds").
			d, err := time.ParseDuration(a.Interval)
			if err != nil {
				return fmt.Sprintf("invalid interval (expected Go duration like '24h'): %v", err), nil
			}
			intervalStr := fmt.Sprintf("%d seconds", int64(d.Seconds()))

			_, err = pool.Exec(ctx,
				`INSERT INTO routines (name, interval_duration, instruction)
				 VALUES ($1, $2::interval, $3)
				 ON CONFLICT (name) DO UPDATE
				 SET interval_duration = $2::interval, instruction = $3, last_executed = now()`,
				a.Name, intervalStr, a.Instruction)
			if err != nil {
				return fmt.Sprintf("failed to upsert routine: %v", err), nil
			}
			return fmt.Sprintf("Routine %q upserted: runs every %s.", a.Name, d), nil

		case "delete":
			tag, err := pool.Exec(ctx,
				`DELETE FROM routines WHERE name = $1`, a.Name)
			if err != nil {
				return fmt.Sprintf("failed to delete routine: %v", err), nil
			}
			if tag.RowsAffected() == 0 {
				return fmt.Sprintf("No routine named %q found.", a.Name), nil
			}
			return fmt.Sprintf("Routine %q deleted.", a.Name), nil

		default:
			return "error: action must be 'upsert' or 'delete'", nil
		}
	}
}
