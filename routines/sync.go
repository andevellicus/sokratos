package routines

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/timeouts"
)

// SyncFromFile does a full sync: reads a routines TOML file, upserts all
// entries into the DB, and deletes any DB routines not present in the file.
// The TOML file is the source of truth.
func SyncFromFile(pool *pgxpool.Pool, path string) (added, updated, deleted []string) {
	if pool == nil {
		return
	}

	routines, err := LoadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Log.Warnf("[routines] failed to load %s: %v", path, err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
	defer cancel()

	fileNames := make(map[string]struct{}, len(routines))
	for name, r := range routines {
		fileNames[name] = struct{}{}

		if err := validateEntry(name, r); err != nil {
			logger.Log.Warnf("[routines] %s", err)
			continue
		}

		tag, err := upsertEntry(ctx, pool, name, r)
		if err != nil {
			logger.Log.Warnf("[routines] failed to upsert %s: %v", name, err)
			continue
		}
		if tag > 0 {
			updated = append(updated, name)
		}
	}

	// Delete DB routines not in the file (TOML is source of truth).
	rows, err := pool.Query(ctx, `SELECT name FROM routines`)
	if err != nil {
		logger.Log.Warnf("[routines] failed to list DB routines: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if _, ok := fileNames[name]; !ok {
			if _, err := pool.Exec(ctx, `DELETE FROM routines WHERE name = $1`, name); err != nil {
				logger.Log.Warnf("[routines] failed to delete stale routine %s: %v", name, err)
			} else {
				deleted = append(deleted, name)
			}
		}
	}

	return
}

// SyncIfChanged checks a routines file for mtime changes and performs a full
// sync from file -> DB when the file has been modified.
func SyncIfChanged(pool *pgxpool.Pool, path string, lastMtime *time.Time) {
	if pool == nil {
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		return
	}
	mtime := info.ModTime()

	if lastMtime.IsZero() {
		*lastMtime = mtime
		return
	}
	if !mtime.After(*lastMtime) {
		return
	}

	added, updated, deleted := SyncFromFile(pool, path)
	*lastMtime = mtime

	total := len(added) + len(updated) + len(deleted)
	if total > 0 {
		logger.Log.Infof("[routines] hot-reload: added %v, updated %v, deleted %v", added, updated, deleted)
	}
}

// Upsert inserts or updates a single routine in the database.
func Upsert(pool *pgxpool.Pool, name string, entry Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
	defer cancel()

	if err := validateEntry(name, entry); err != nil {
		return err
	}

	_, err := upsertEntry(ctx, pool, name, entry)
	return err
}

// Delete removes a routine from the database.
func Delete(pool *pgxpool.Pool, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
	defer cancel()

	_, err := pool.Exec(ctx, `DELETE FROM routines WHERE name = $1`, name)
	return err
}

// QueryDue returns routines that are candidates for execution: interval-based
// routines whose timer has elapsed, and schedule-based routines where at least
// one schedule time has been reached today.
func QueryDue(pool *pgxpool.Pool) ([]DueRoutine, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()

	rows, err := pool.Query(ctx,
		`SELECT id, name, instruction, action, actions, goal,
		        COALESCE(silent_if_empty, false), schedule, last_executed, action_args,
		        (interval_duration IS NOT NULL AND last_executed + interval_duration <= NOW()) AS interval_due
		 FROM routines
		 WHERE (interval_duration IS NOT NULL AND last_executed + interval_duration <= NOW())
		    OR schedule IS NOT NULL
		 ORDER BY last_executed ASC`)
	if err != nil {
		return nil, fmt.Errorf("query due routines: %w", err)
	}
	defer rows.Close()

	var result []DueRoutine
	for rows.Next() {
		var d DueRoutine
		var schedule *string
		var actionArgsJSON []byte
		var intervalDue bool

		if err := rows.Scan(&d.ID, &d.Name, &d.Instruction, &d.Action, &d.Actions, &d.Goal,
			&d.SilentIfEmpty, &schedule, &d.LastExecuted, &actionArgsJSON, &intervalDue); err != nil {
			logger.Log.Warnf("[routines] failed to scan routine row: %v", err)
			continue
		}

		// Parse schedules from comma-separated column.
		if schedule != nil {
			d.Schedules = ParseSchedules(*schedule)
		}

		// Parse action_args JSONB column.
		if len(actionArgsJSON) > 0 {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(actionArgsJSON, &raw); err == nil {
				d.ActionArgs = raw
			}
		}

		// A routine is due if its interval fired OR any schedule time matches.
		scheduleDue := len(d.Schedules) > 0 && IsScheduleDue(d.Schedules, d.LastExecuted)
		if intervalDue || scheduleDue {
			result = append(result, d)
		}
	}

	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("routine iteration error: %w", err)
	}

	return result, nil
}

// AdvanceTimer updates the last_executed timestamp for a routine.
func AdvanceTimer(pool *pgxpool.Pool, id int) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()

	_, err := pool.Exec(ctx, `UPDATE routines SET last_executed = NOW() WHERE id = $1`, id)
	return err
}

// validateEntry checks that a routine entry has valid configuration.
func validateEntry(name string, r Entry) error {
	schedStr := NormalizeSchedule(r.Schedule)

	// At least one trigger required.
	if r.Interval == "" && schedStr == "" {
		return fmt.Errorf("%q: missing interval or schedule, skipping", name)
	}

	// Validate schedule entries.
	if schedStr != "" {
		if err := ValidateSchedules(r.Schedule); err != nil {
			return fmt.Errorf("%q: %v, skipping", name, err)
		}
	}

	// At least one action required.
	if r.Action == "" && len(r.Actions) == 0 && r.Instruction == "" {
		return fmt.Errorf("%q: missing action, actions, or instruction, skipping", name)
	}

	return nil
}

// upsertEntry performs the DB upsert for a single routine. Returns rows affected.
func upsertEntry(ctx context.Context, pool *pgxpool.Pool, name string, r Entry) (int64, error) {
	instruction := r.Instruction
	if (r.Action != "" || len(r.Actions) > 0) && instruction == "" {
		instruction = r.Goal
	}

	var intervalArg interface{}
	if r.Interval != "" {
		intervalArg = r.Interval
	}

	var actionsArg interface{}
	if len(r.Actions) > 0 {
		actionsArg = r.Actions
	}

	schedStr := NormalizeSchedule(r.Schedule)

	// Marshal action_args to JSONB.
	var actionArgsArg interface{}
	if len(r.ActionArgs) > 0 {
		b, err := json.Marshal(r.ActionArgs)
		if err == nil {
			actionArgsArg = b
		}
	}

	tag, err := pool.Exec(ctx,
		`INSERT INTO routines (name, interval_duration, instruction, action, goal, silent_if_empty, schedule, actions, action_args)
		 VALUES ($1, $2::interval, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (name) DO UPDATE SET
		   interval_duration = EXCLUDED.interval_duration,
		   instruction = EXCLUDED.instruction,
		   action = EXCLUDED.action,
		   goal = EXCLUDED.goal,
		   silent_if_empty = EXCLUDED.silent_if_empty,
		   schedule = EXCLUDED.schedule,
		   actions = EXCLUDED.actions,
		   action_args = EXCLUDED.action_args`,
		name, intervalArg, instruction, NilIfEmpty(r.Action), NilIfEmpty(r.Goal), r.SilentIfEmpty,
		NilIfEmpty(schedStr), actionsArg, actionArgsArg)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
