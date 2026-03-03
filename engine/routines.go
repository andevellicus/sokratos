package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/routines"
	"sokratos/textutil"
)

// runRoutineScheduler polls for due routines on its own ticker, independent
// of the heartbeat loop. This gives routines better time precision (default
// 30s polling) and decouples them from heartbeat processing.
func (e *Engine) runRoutineScheduler() {
	interval := e.RoutineInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Log.Infof("[routine-scheduler] started (interval: %s)", interval)

	for range ticker.C {
		// Hot-reload routines from disk before checking for due routines.
		if e.RoutineSyncFunc != nil {
			e.RoutineSyncFunc()
		}
		e.executeDueRoutines()
	}
}

// executeDueRoutines queries all routines whose interval has elapsed or
// schedule time has been reached, and executes each one independently
// through the orchestrator.
func (e *Engine) executeDueRoutines() {
	directives, err := routines.QueryDue(e.DB)
	if err != nil {
		logger.Log.Warnf("routine-scheduler: %v", err)
		return
	}

	for _, d := range directives {
		e.executeSingleRoutine(d)
	}

	if len(directives) > 0 {
		logger.Log.Infof("routine-scheduler: executed %d routine(s)", len(directives))
	}
}

// executeSingleRoutine advances the routine's timer and runs it through
// the orchestrator. If the routine has a `tool` field, the tool is called
// directly first and the result is passed to the orchestrator with the goal.
// If silent_if_empty is set and the tool returns no data, the orchestrator
// is skipped entirely. Execution is tracked via WorkMonitor and bounded by
// RoutineTimeout.
func (e *Engine) executeSingleRoutine(d routines.DueRoutine) {
	defer func() {
		if r := recover(); r != nil {
			logger.Log.Errorf("routine-scheduler: panic, name=%s, err=%v", d.Name, r)
		}
	}()

	// Advance timer BEFORE execution to prevent double-fire on crash.
	if err := routines.AdvanceTimer(e.DB, d.ID); err != nil {
		logger.Log.Warnf("routine-scheduler: failed to advance timer, name=%s, err=%v", d.Name, err)
		return
	}

	// Compute timeout and create a cancellable context.
	timeout := e.RoutineTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Track via WorkMonitor if available.
	var workID int64
	if e.WorkMonitor != nil {
		workID = e.WorkMonitor.TrackStart("routine", d.Name, timeout)
		e.WorkMonitor.SetCancel(workID, cancel)
		defer func() {
			status := "completed"
			var errMsg string
			if ctx.Err() != nil {
				status = "failed"
				errMsg = ctx.Err().Error()
			}
			e.WorkMonitor.TrackEnd(workID, status, errMsg)
		}()
	}

	var prompt string

	// Resolve tool list with precedence: Tools (multi) > Tool (single) > legacy instruction.
	var toolList []string
	if len(d.Tools) > 0 {
		toolList = d.Tools
	} else if d.Tool != nil && *d.Tool != "" {
		toolList = []string{*d.Tool}
	}

	if len(toolList) > 0 {
		// Structured routine: call tool(s) directly, then hand results to the orchestrator.
		var results strings.Builder
		anyNonEmpty := false

		for _, toolName := range toolList {
			argsJSON := json.RawMessage("{}")
			if d.ToolArgs != nil {
				if ta, ok := d.ToolArgs[toolName]; ok {
					argsJSON = routines.ExpandAndMarshal(ta)
				}
			}
			toolResult, err := e.ToolExec(ctx, json.RawMessage(
				fmt.Sprintf(`{"name":%q,"arguments":%s}`, toolName, argsJSON),
			))
			if err != nil {
				logger.Log.Warnf("routine-scheduler: tool call failed, name=%s, tool=%s, err=%v", d.Name, toolName, err)
				continue
			}
			if !routines.IsEmptyResult(toolResult) {
				anyNonEmpty = true
			}
			fmt.Fprintf(&results, "## %s\n%s\n\n", toolName, toolResult)
		}

		// If silent_if_empty and ALL tools returned empty, skip orchestrator.
		if d.SilentIfEmpty && !anyNonEmpty {
			logger.Log.Infof("routine-scheduler: %s: all tools returned empty, skipping (silent)", d.Name)
			return
		}

		goal := d.Instruction
		if d.Goal != nil && *d.Goal != "" {
			goal = *d.Goal
		}

		toolLabel := strings.Join(toolList, ", ")
		prompt = fmt.Sprintf(
			"ROUTINE: %s\nThe following tools were called: %s\n\nResults:\n%s\nYour task: %s",
			d.Name, toolLabel, results.String(), goal,
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

	var reply string
	var err error
	e.withOrchestratorLock(func() {
		reply, _, err = llm.QueryOrchestrator(
			ctx, e.LLM.Client, e.LLM.Model, prompt,
			e.ToolExec, DefaultTrimFn, e.baseOrchestratorOpts(),
		)
	})

	if err != nil {
		logger.Log.Warnf("routine-scheduler: routine failed, name=%s, err=%v", d.Name, err)
		return
	}

	reply = strings.TrimSpace(reply)
	if reply != "" && !strings.Contains(reply, "<NO_ACTION_REQUIRED>") {
		if e.sendDeduped(reply, fmt.Sprintf("routine %q", d.Name)) {
			e.recordAction("routine", fmt.Sprintf("Sent %q output: %s", d.Name, textutil.Truncate(reply, 80)))
			logger.Log.Infof("routine-scheduler: %q delivered", d.Name)
		}
	} else {
		logger.Log.Infof("routine-scheduler: %q executed (no user-facing output)", d.Name)
		e.recordAction("routine", fmt.Sprintf("Executed %q (silent)", d.Name))
	}
}
