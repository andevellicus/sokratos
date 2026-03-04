package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/routines"
)

type routineArgs struct {
	Op            string      `json:"op"`                          // "upsert" or "delete"
	Name          string      `json:"name"`                        // unique routine name
	Interval      string      `json:"interval,omitempty"`          // Go duration, e.g. "24h", "1h"
	Schedule      interface{} `json:"schedule,omitempty"`          // string or []string
	Action        string      `json:"action,omitempty"`            // action to call directly
	Actions       []string    `json:"actions,omitempty"`           // multi-action list (mutually exclusive with action)
	ActionArgs    map[string]map[string]interface{} `json:"action_args,omitempty"` // per-action arguments
	Goal          string      `json:"goal,omitempty"`              // what to do with action results
	SilentIfEmpty bool        `json:"silent_if_empty,omitempty"`   // skip if action returns empty
	Instruction   string      `json:"instruction,omitempty"`       // legacy: full instruction text
}

// NewManageRoutines returns a ToolFunc that creates, updates, or deletes
// autonomous routine items in the PostgreSQL routines table. When fileWriter
// is non-nil, changes are also written back to the routines TOML file.
func NewManageRoutines(pool *pgxpool.Pool, fileWriter routines.FileWriter) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a routineArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Name == "" {
			return "error: name is required", nil
		}

		switch a.Op {
		case "upsert":
			if a.Action == "" && len(a.Actions) == 0 && a.Instruction == "" {
				return "error: action, actions, or instruction is required for upsert", nil
			}

			schedStr := routines.NormalizeSchedule(a.Schedule)

			// At least one trigger required.
			if a.Interval == "" && schedStr == "" {
				return "error: interval or schedule is required for upsert", nil
			}

			// Validate schedule entries.
			if schedStr != "" {
				if err := routines.ValidateSchedules(a.Schedule); err != nil {
					return err.Error(), nil
				}
			}

			// Parse interval if provided.
			var intervalArg interface{}
			var displayStr string
			if a.Interval != "" {
				d, err := time.ParseDuration(a.Interval)
				if err != nil {
					return fmt.Sprintf("invalid interval (expected Go duration like '24h'): %v", err), nil
				}
				intervalArg = fmt.Sprintf("%d seconds", int64(d.Seconds()))
				displayStr = fmt.Sprintf("runs every %s", d)
			}
			if schedStr != "" {
				if displayStr != "" {
					displayStr += fmt.Sprintf(" + daily at %s", schedStr)
				} else {
					displayStr = fmt.Sprintf("runs daily at %s", schedStr)
				}
			}

			// Build instruction from goal for backward compat.
			instruction := a.Instruction
			if (a.Action != "" || len(a.Actions) > 0) && instruction == "" {
				instruction = a.Goal
			}

			var actionsArg interface{}
			if len(a.Actions) > 0 {
				actionsArg = a.Actions
			}

			// Marshal action_args to JSONB.
			var actionArgsArg interface{}
			if len(a.ActionArgs) > 0 {
				b, err := json.Marshal(a.ActionArgs)
				if err == nil {
					actionArgsArg = b
				}
			}

			_, err := pool.Exec(ctx,
				`INSERT INTO routines (name, interval_duration, instruction, action, goal, silent_if_empty, schedule, actions, action_args)
				 VALUES ($1, $2::interval, $3, $4, $5, $6, $7, $8, $9)
				 ON CONFLICT (name) DO UPDATE
				 SET interval_duration = $2::interval, instruction = $3, action = $4, goal = $5,
				     silent_if_empty = $6, schedule = $7, actions = $8, action_args = $9, last_executed = now()`,
				a.Name, intervalArg, instruction, routines.NilIfEmpty(a.Action), routines.NilIfEmpty(a.Goal),
				a.SilentIfEmpty, routines.NilIfEmpty(schedStr), actionsArg, actionArgsArg)
			if err != nil {
				return fmt.Sprintf("failed to upsert routine: %v", err), nil
			}

			// Write back to TOML file (source of truth).
			if fileWriter != nil {
				entry := routines.Entry{
					Interval:      a.Interval,
					Schedule:      a.Schedule,
					Action:        a.Action,
					Actions:       a.Actions,
					ActionArgs:    a.ActionArgs,
					Goal:          a.Goal,
					SilentIfEmpty: a.SilentIfEmpty,
					Instruction:   a.Instruction,
				}
				if err := fileWriter.Write(a.Name, entry); err != nil {
					logger.Log.Warnf("[routines] file write-back failed for %s: %v", a.Name, err)
				}
			}

			return fmt.Sprintf("Routine %q upserted: %s.", a.Name, displayStr), nil

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
				fileWriter.Delete(a.Name)
			}

			return fmt.Sprintf("Routine %q deleted.", a.Name), nil

		default:
			return "error: op must be 'upsert' or 'delete'", nil
		}
	}
}
