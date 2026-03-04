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
// the orchestrator. If the routine has an `action` field, the action is called
// directly first and the result is passed to the orchestrator with the goal.
// If silent_if_empty is set and the action returns no data, the orchestrator
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

	// Resolve action list with precedence: Actions (multi) > Action (single) > legacy instruction.
	var actionList []string
	if len(d.Actions) > 0 {
		actionList = d.Actions
	} else if d.Action != nil && *d.Action != "" {
		actionList = []string{*d.Action}
	}

	if len(actionList) > 0 {
		// Structured routine: call action(s) directly, then hand results to the orchestrator.
		var results strings.Builder
		anyNonEmpty := false

		for _, actionName := range actionList {
			argsJSON := json.RawMessage("{}")
			if d.ActionArgs != nil {
				if ta, ok := d.ActionArgs[actionName]; ok {
					argsJSON = routines.ExpandAndMarshal(ta)
				}
			}
			actionResult, err := e.ToolExec(ctx, json.RawMessage(
				fmt.Sprintf(`{"name":%q,"arguments":%s}`, actionName, argsJSON),
			))
			if err != nil {
				logger.Log.Warnf("routine-scheduler: action call failed, name=%s, action=%s, err=%v", d.Name, actionName, err)
				continue
			}
			if !routines.IsEmptyResult(actionResult) {
				anyNonEmpty = true
			}
			fmt.Fprintf(&results, "## %s\n%s\n\n", actionName, actionResult)
		}

		// If silent_if_empty and ALL actions returned empty, skip orchestrator.
		if d.SilentIfEmpty && !anyNonEmpty {
			logger.Log.Infof("routine-scheduler: %s: all actions returned empty, skipping (silent)", d.Name)
			return
		}

		goal := d.Instruction
		if d.Goal != nil && *d.Goal != "" {
			goal = *d.Goal
		}

		actionLabel := strings.Join(actionList, ", ")
		prompt = fmt.Sprintf(
			"ROUTINE: %s\nThe following actions were called: %s\n\nResults:\n%s\nYour task: %s",
			d.Name, actionLabel, results.String(), goal,
		)
	} else {
		// Legacy routine: pass instruction to orchestrator and let it figure things out.
		prompt = fmt.Sprintf(
			"ROUTINE: %s\nExecute this routine now. Use your tools to complete it.\n"+
				"Do not message the user unless the routine explicitly requires it.",
			d.Instruction,
		)
	}

	// Prepend routine mode preamble and user preferences for focused execution context.
	preamble := strings.TrimSpace(prompts.RoutineMode)
	if prefs := e.SM.GetState().UserPrefs; len(prefs) > 0 {
		var pb strings.Builder
		pb.WriteString("\n\nUser preferences (highest priority — override general rules):\n")
		for k, v := range prefs {
			fmt.Fprintf(&pb, "- %s: %s\n", k, v)
		}
		preamble += pb.String()
	}
	prompt = preamble + "\n\n" + prompt

	reply, _, err := e.runOrchestrator(ctx, false, prompt, nil)

	if err != nil {
		logger.Log.Warnf("routine-scheduler: routine failed, name=%s, err=%v", d.Name, err)
		return
	}

	reply = strings.TrimSpace(reply)
	if reply != "" && !strings.Contains(reply, "<NO_ACTION_REQUIRED>") {
		if e.sendDeduped(reply, fmt.Sprintf("routine %q", d.Name)) {
			e.recordAction("routine", fmt.Sprintf("Sent %q output: %s", d.Name, textutil.Truncate(reply, 80)))
			// Persist condensed output in conversation state so the interactive
			// orchestrator can reference what was just sent to the user.
			e.SM.AppendMessage(llm.Message{
				Role:    "assistant",
				Content: fmt.Sprintf("[ROUTINE: %s]\n%s", d.Name, textutil.Truncate(reply, 500)),
			})
			logger.Log.Infof("routine-scheduler: %q delivered", d.Name)
		}
	} else {
		logger.Log.Infof("routine-scheduler: %q executed (no user-facing output)", d.Name)
		e.recordAction("routine", fmt.Sprintf("Executed %q (silent)", d.Name))
	}
}
