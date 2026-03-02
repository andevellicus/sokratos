package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
)

type routineArgs struct {
	Action        string `json:"action"`                    // "upsert" or "delete"
	Name          string `json:"name"`                      // unique routine name
	Interval      string `json:"interval,omitempty"`        // Go duration, e.g. "24h", "1h"
	Tool          string `json:"tool,omitempty"`            // tool to call directly
	Goal          string `json:"goal,omitempty"`            // what to do with tool results
	SilentIfEmpty bool   `json:"silent_if_empty,omitempty"` // skip if tool returns empty
	Instruction   string `json:"instruction,omitempty"`     // legacy: full instruction text
}

// RoutineEntry mirrors the main package's routineEntry for file write-back.
type RoutineEntry struct {
	Interval      string
	Tool          string
	Goal          string
	SilentIfEmpty bool
	Instruction   string
}

// RoutineFileWriter writes routine entries to the routines TOML file.
type RoutineFileWriter interface {
	WriteRoutine(name string, entry RoutineEntry) error
	DeleteRoutine(name string)
}

// NewManageRoutines returns a ToolFunc that creates, updates, or deletes
// autonomous routine items in the PostgreSQL routines table. When fileWriter
// is non-nil, changes are also written back to the routines TOML file.
func NewManageRoutines(pool *pgxpool.Pool, fileWriter RoutineFileWriter) ToolFunc {
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
			if a.Tool == "" && a.Instruction == "" {
				return "error: tool or instruction is required for upsert", nil
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

			// Build instruction from goal for backward compat.
			instruction := a.Instruction
			if a.Tool != "" && instruction == "" {
				instruction = a.Goal
			}

			var toolPtr, goalPtr *string
			if a.Tool != "" {
				toolPtr = &a.Tool
			}
			if a.Goal != "" {
				goalPtr = &a.Goal
			}

			_, err = pool.Exec(ctx,
				`INSERT INTO routines (name, interval_duration, instruction, tool, goal, silent_if_empty)
				 VALUES ($1, $2::interval, $3, $4, $5, $6)
				 ON CONFLICT (name) DO UPDATE
				 SET interval_duration = $2::interval, instruction = $3, tool = $4, goal = $5,
				     silent_if_empty = $6, last_executed = now()`,
				a.Name, intervalStr, instruction, toolPtr, goalPtr, a.SilentIfEmpty)
			if err != nil {
				return fmt.Sprintf("failed to upsert routine: %v", err), nil
			}

			// Write back to TOML file (source of truth).
			if fileWriter != nil {
				entry := RoutineEntry{
					Interval:      a.Interval,
					Tool:          a.Tool,
					Goal:          a.Goal,
					SilentIfEmpty: a.SilentIfEmpty,
					Instruction:   a.Instruction,
				}
				if err := fileWriter.WriteRoutine(a.Name, entry); err != nil {
					logger.Log.Warnf("[routines] file write-back failed for %s: %v", a.Name, err)
				}
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

			// Remove from TOML file.
			if fileWriter != nil {
				fileWriter.DeleteRoutine(a.Name)
			}

			return fmt.Sprintf("Routine %q deleted.", a.Name), nil

		default:
			return "error: action must be 'upsert' or 'delete'", nil
		}
	}
}
