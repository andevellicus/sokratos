package engine

import (
	"context"
	"fmt"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/timeouts"
)

// executeDueRoutines queries all directives whose interval has elapsed and
// executes each one independently through the orchestrator. Returns the count
// of directives fired.
func (e *Engine) executeDueRoutines() int {
	queryCtx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()

	rows, err := e.DB.Query(queryCtx,
		`SELECT id, name, instruction
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
		if err := rows.Scan(&d.ID, &d.Name, &d.Instruction); err != nil {
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
// the full orchestrator/supervisor loop. Recovers from panics so a single
// failed directive never crashes the heartbeat goroutine.
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

	prompt := fmt.Sprintf(
		"ROUTINE: %s\nExecute this routine now. Use your tools to complete it.\n"+
			"Do not message the user unless the routine explicitly requires it.",
		d.Instruction,
	)

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
}
