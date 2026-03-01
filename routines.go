package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sokratos/db"
	"sokratos/logger"
	"sokratos/timeouts"
)

// seedDefaultRoutines loads routine definitions from the routines/ directory
// and upserts them into the database. Each .txt file has the format:
//
//	interval: 4 hours
//	---
//	instruction text here
//
// The filename (without .txt) becomes the routine name. Uses ON CONFLICT
// DO UPDATE so editing the file and restarting always applies the change.
func seedDefaultRoutines() {
	files, err := filepath.Glob("routines/*.txt")
	if err != nil || len(files) == 0 {
		if err != nil {
			logger.Log.Warnf("[startup] failed to glob routines: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
	defer cancel()

	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".txt")
		data, err := os.ReadFile(f)
		if err != nil {
			logger.Log.Warnf("[startup] failed to read routine %s: %v", name, err)
			continue
		}

		interval, instruction, err := parseRoutineFile(string(data))
		if err != nil {
			logger.Log.Warnf("[startup] failed to parse routine %s: %v", name, err)
			continue
		}

		_, err = db.Pool.Exec(ctx,
			`INSERT INTO routines (name, interval_duration, instruction)
			 VALUES ($1, $2::interval, $3)
			 ON CONFLICT (name) DO UPDATE SET
			   interval_duration = EXCLUDED.interval_duration,
			   instruction = EXCLUDED.instruction`,
			name, interval, instruction)
		if err != nil {
			logger.Log.Warnf("[startup] failed to seed routine %s: %v", name, err)
		}
	}
}

// parseRoutineFile splits a routine file into interval and instruction.
// Expected format: "interval: <value>\n---\n<instruction>"
func parseRoutineFile(content string) (interval, instruction string, err error) {
	parts := strings.SplitN(content, "---", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("missing --- separator")
	}

	header := strings.TrimSpace(parts[0])
	instruction = strings.TrimSpace(parts[1])

	if instruction == "" {
		return "", "", fmt.Errorf("empty instruction")
	}

	prefix, found := strings.CutPrefix(header, "interval:")
	if !found {
		return "", "", fmt.Errorf("missing interval: header")
	}
	interval = strings.TrimSpace(prefix)
	if interval == "" {
		return "", "", fmt.Errorf("empty interval value")
	}

	return interval, instruction, nil
}

// syncRoutineFiles checks routines/*.txt files for mtime changes and upserts
// any modified routines into the database. The mtimeCache tracks previously
// seen modification times to avoid unnecessary writes.
func syncRoutineFiles(routinesDir string, mtimeCache map[string]time.Time) {
	if db.Pool == nil {
		return
	}

	files, err := filepath.Glob(filepath.Join(routinesDir, "*.txt"))
	if err != nil || len(files) == 0 {
		return
	}

	var updated []string
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		mtime := info.ModTime()
		if _, ok := mtimeCache[f]; !ok {
			// First sync after startup — just seed the cache without re-upserting
			// (seedDefaultRoutines already wrote these on boot).
			mtimeCache[f] = mtime
			continue
		} else if !mtime.After(mtimeCache[f]) {
			continue
		}

		name := strings.TrimSuffix(filepath.Base(f), ".txt")
		data, err := os.ReadFile(f)
		if err != nil {
			logger.Log.Warnf("[routines] hot-reload: failed to read %s: %v", name, err)
			continue
		}

		interval, instruction, err := parseRoutineFile(string(data))
		if err != nil {
			logger.Log.Warnf("[routines] hot-reload: failed to parse %s: %v", name, err)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
		_, err = db.Pool.Exec(ctx,
			`INSERT INTO routines (name, interval_duration, instruction)
			 VALUES ($1, $2::interval, $3)
			 ON CONFLICT (name) DO UPDATE SET
			   interval_duration = EXCLUDED.interval_duration,
			   instruction = EXCLUDED.instruction`,
			name, interval, instruction)
		cancel()

		if err != nil {
			logger.Log.Warnf("[routines] hot-reload: failed to upsert %s: %v", name, err)
			continue
		}

		mtimeCache[f] = mtime
		updated = append(updated, name)
	}

	if len(updated) > 0 {
		logger.Log.Infof("[routines] hot-reload: updated %v", updated)
	}
}
