package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"sokratos/db"
	"sokratos/logger"
	"sokratos/timeouts"
	"sokratos/tools"
)

// routineEntry is the TOML structure for a single routine within routines.toml.
type routineEntry struct {
	Interval      string `toml:"interval"`
	Tool          string `toml:"tool,omitempty"`
	Goal          string `toml:"goal,omitempty"`
	SilentIfEmpty bool   `toml:"silent_if_empty,omitempty"`
	Instruction   string `toml:"instruction,omitempty"` // legacy fallback
}

// routinesFileMu protects concurrent reads/writes to the routines TOML file.
var routinesFileMu sync.Mutex

// nilIfEmpty returns nil for empty strings (maps to SQL NULL).
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// loadRoutinesFile reads and parses routines.toml into a map of name → entry.
func loadRoutinesFile(path string) (map[string]routineEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var routines map[string]routineEntry
	if err := toml.Unmarshal(data, &routines); err != nil {
		return nil, fmt.Errorf("invalid TOML: %w", err)
	}
	return routines, nil
}

// SyncRoutinesFromFile does a full sync: reads routines.toml, upserts all
// entries into the DB, and deletes any DB routines not present in the file.
// The TOML file is the source of truth.
func SyncRoutinesFromFile(path string) (added, updated, deleted []string) {
	if db.Pool == nil {
		return
	}

	routines, err := loadRoutinesFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Log.Warnf("[routines] failed to load %s: %v", path, err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
	defer cancel()

	// Upsert all routines from the file.
	fileNames := make(map[string]struct{}, len(routines))
	for name, r := range routines {
		fileNames[name] = struct{}{}

		if r.Interval == "" {
			logger.Log.Warnf("[routines] %q: missing interval, skipping", name)
			continue
		}
		if r.Tool == "" && r.Instruction == "" {
			logger.Log.Warnf("[routines] %q: missing tool or instruction, skipping", name)
			continue
		}

		instruction := r.Instruction
		if r.Tool != "" && instruction == "" {
			instruction = r.Goal
		}

		tag, err := db.Pool.Exec(ctx,
			`INSERT INTO routines (name, interval_duration, instruction, tool, goal, silent_if_empty)
			 VALUES ($1, $2::interval, $3, $4, $5, $6)
			 ON CONFLICT (name) DO UPDATE SET
			   interval_duration = EXCLUDED.interval_duration,
			   instruction = EXCLUDED.instruction,
			   tool = EXCLUDED.tool,
			   goal = EXCLUDED.goal,
			   silent_if_empty = EXCLUDED.silent_if_empty`,
			name, r.Interval, instruction, nilIfEmpty(r.Tool), nilIfEmpty(r.Goal), r.SilentIfEmpty)
		if err != nil {
			logger.Log.Warnf("[routines] failed to upsert %s: %v", name, err)
			continue
		}
		if tag.RowsAffected() > 0 {
			updated = append(updated, name)
		}
	}

	// Delete DB routines not in the file (TOML is source of truth).
	rows, err := db.Pool.Query(ctx, `SELECT name FROM routines`)
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
			if _, err := db.Pool.Exec(ctx, `DELETE FROM routines WHERE name = $1`, name); err != nil {
				logger.Log.Warnf("[routines] failed to delete stale routine %s: %v", name, err)
			} else {
				deleted = append(deleted, name)
			}
		}
	}

	return
}

// syncRoutinesFile checks routines.toml for mtime changes and performs a full
// sync from file → DB when the file has been modified.
func syncRoutinesFile(path string, lastMtime *time.Time) {
	if db.Pool == nil {
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

	added, updated, deleted := SyncRoutinesFromFile(path)
	*lastMtime = mtime

	total := len(added) + len(updated) + len(deleted)
	if total > 0 {
		logger.Log.Infof("[routines] hot-reload: added %v, updated %v, deleted %v", added, updated, deleted)
	}
}

// WriteRoutineToFile adds or updates a routine in routines.toml.
func WriteRoutineToFile(path string, entry routineEntry, name string) error {
	routinesFileMu.Lock()
	defer routinesFileMu.Unlock()

	routines, err := loadRoutinesFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read existing: %w", err)
	}
	if routines == nil {
		routines = make(map[string]routineEntry)
	}

	routines[name] = entry
	return writeRoutinesFile(path, routines)
}

// DeleteRoutineFromFile removes a routine from routines.toml.
func DeleteRoutineFromFile(path, name string) {
	routinesFileMu.Lock()
	defer routinesFileMu.Unlock()

	routines, err := loadRoutinesFile(path)
	if err != nil {
		return
	}
	delete(routines, name)
	if err := writeRoutinesFile(path, routines); err != nil {
		logger.Log.Warnf("[routines] failed to write file after delete: %v", err)
	}
}

// writeRoutinesFile serializes routines to TOML and writes to disk.
func writeRoutinesFile(path string, routines map[string]routineEntry) error {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(routines); err != nil {
		return fmt.Errorf("encode TOML: %w", err)
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

// routineFileAdapter implements tools.RoutineFileWriter by delegating to the
// package-level WriteRoutineToFile/DeleteRoutineFromFile functions.
type routineFileAdapter struct {
	path string
}

func (a *routineFileAdapter) WriteRoutine(name string, entry tools.RoutineEntry) error {
	return WriteRoutineToFile(a.path, routineEntry{
		Interval:      entry.Interval,
		Tool:          entry.Tool,
		Goal:          entry.Goal,
		SilentIfEmpty: entry.SilentIfEmpty,
		Instruction:   entry.Instruction,
	}, name)
}

func (a *routineFileAdapter) DeleteRoutine(name string) {
	DeleteRoutineFromFile(a.path, name)
}
