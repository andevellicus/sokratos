package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/timeouts"
)

// executeDueRoutines queries all directives whose interval has elapsed and
// executes each one independently through the orchestrator. Returns the count
// of directives fired.
func (e *Engine) executeDueRoutines() int {
	queryCtx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()

	rows, err := e.DB.Query(queryCtx,
		`SELECT id, name, instruction, tool, goal, COALESCE(silent_if_empty, false)
		 FROM routines
		 WHERE last_executed + interval_duration <= NOW()
		 ORDER BY (last_executed + interval_duration) ASC`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query due routines: %v", err)
		return 0
	}

	var directives []dueRoutine
	for rows.Next() {
		var d dueRoutine
		if err := rows.Scan(&d.ID, &d.Name, &d.Instruction, &d.Tool, &d.Goal, &d.SilentIfEmpty); err != nil {
			logger.Log.Warnf("heartbeat: failed to scan routine row: %v", err)
			continue
		}
		directives = append(directives, d)
	}
	rows.Close()

	if rows.Err() != nil {
		logger.Log.Warnf("heartbeat: routine iteration error: %v", rows.Err())
	}

	for _, d := range directives {
		e.executeSingleRoutine(d)
	}

	return len(directives)
}

// executeSingleRoutine advances the directive's timer and runs it through
// the orchestrator. If the routine has a `tool` field, the tool is called
// directly first and the result is passed to the orchestrator with the goal.
// If silent_if_empty is set and the tool returns no data, the orchestrator
// is skipped entirely.
func (e *Engine) executeSingleRoutine(d dueRoutine) {
	defer func() {
		if r := recover(); r != nil {
			logger.Log.Errorf("heartbeat: routine panic, name=%s, err=%v", d.Name, r)
		}
	}()

	// Advance timer BEFORE execution to prevent double-fire on crash.
	advCtx, advCancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer advCancel()
	if _, err := e.DB.Exec(advCtx,
		`UPDATE routines SET last_executed = NOW() WHERE id = $1`, d.ID); err != nil {
		logger.Log.Warnf("heartbeat: failed to advance routine timer, name=%s, err=%v", d.Name, err)
		return
	}

	var prompt string

	if d.Tool != nil && *d.Tool != "" {
		// Structured routine: call the tool directly, then hand results to the orchestrator.
		toolResult, err := e.ToolExec(context.Background(), json.RawMessage(
			fmt.Sprintf(`{"name":%q,"arguments":{}}`, *d.Tool),
		))
		if err != nil {
			logger.Log.Warnf("heartbeat: routine tool call failed, name=%s, tool=%s, err=%v", d.Name, *d.Tool, err)
			return
		}

		// If the tool returned nothing useful and silent_if_empty is set, skip.
		if d.SilentIfEmpty && isEmptyToolResult(toolResult) {
			logger.Log.Infof("heartbeat: routine %s: tool returned empty, skipping (silent)", d.Name)
			return
		}

		goal := d.Instruction
		if d.Goal != nil && *d.Goal != "" {
			goal = *d.Goal
		}

		prompt = fmt.Sprintf(
			"ROUTINE: %s\nThe tool %q was called and returned the following data:\n\n%s\n\nYour task: %s",
			d.Name, *d.Tool, toolResult, goal,
		)
	} else {
		// Legacy routine: pass instruction to orchestrator and let it figure things out.
		prompt = fmt.Sprintf(
			"ROUTINE: %s\nExecute this routine now. Use your tools to complete it.\n"+
				"Do not message the user unless the routine explicitly requires it.",
			d.Instruction,
		)
	}

	// Prepend routine mode preamble for focused execution context.
	prompt = strings.TrimSpace(prompts.RoutineMode) + "\n\n" + prompt

	var err error
	e.withOrchestratorLock(func() {
		_, _, err = llm.QueryOrchestrator(
			context.Background(), e.LLM.Client, e.LLM.Model, prompt,
			e.ToolExec, DefaultTrimFn, e.baseOrchestratorOpts(),
		)
	})

	if err != nil {
		logger.Log.Warnf("heartbeat: routine failed, name=%s, err=%v", d.Name, err)
		return
	}

	logger.Log.Infof("heartbeat: routine executed, name=%s", d.Name)
	e.recordAction("routine", fmt.Sprintf("Executed %q", d.Name))
}

// isEmptyToolResult checks if a tool result indicates no data was returned.
func isEmptyToolResult(result string) bool {
	if result == "" {
		return true
	}
	// Skills return "No tweets found.", "No news articles found.", etc.
	for _, prefix := range []string{"No ", "no ", "error", "Error"} {
		if len(result) > len(prefix) && result[:len(prefix)] == prefix {
			return true
		}
	}
	// JSON results with count=0
	if len(result) > 10 && result[0] == '{' {
		if idx := indexOf(result, `"count":0`); idx >= 0 {
			return true
		}
	}
	return false
}

// indexOf returns the index of substr in s, or -1 if not found.
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
