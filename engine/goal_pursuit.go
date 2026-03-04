package engine

import (
	"context"
	"fmt"
	"time"

	"sokratos/logger"
	"sokratos/timeouts"
)

const (
	goalPursuitCooldown    = 8 * time.Hour
	goalPursuitMaxInactive = 24 * time.Hour
)

// runGoalPursuitIfReady selects the highest-salience active goal that hasn't
// been attempted recently and launches a background task for it. This bypasses
// the gatekeeper's "none" bias to ensure goals are actively worked on.
func (e *Engine) runGoalPursuitIfReady() {
	if e.Cognitive.CuriosityFunc == nil || e.DB == nil {
		return
	}

	// Cooldown check.
	if time.Since(e.lastGoalPursuitRun) < goalPursuitCooldown {
		return
	}

	// User activity check — only pursue goals when the user was recently active.
	lastActivity := e.SM.LastUserActivity()
	if lastActivity.IsZero() || time.Since(lastActivity) > goalPursuitMaxInactive {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()

	// Check for already-running goal task.
	var running int
	if err := e.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_items
		 WHERE status = 'running' AND directive LIKE '[goal]%'`).Scan(&running); err == nil && running > 0 {
		logger.Log.Debug("[goal-pursuit] task already running, skipping")
		return
	}

	// Query active goals (same criteria as heartbeat).
	goalRows, err := e.DB.Query(ctx,
		`SELECT id, summary FROM memories
		 WHERE memory_type = 'goal'
		   AND superseded_by IS NULL
		   AND salience >= 6
		   AND created_at >= NOW() - INTERVAL '14 days'
		 ORDER BY salience DESC, created_at DESC
		 LIMIT 3`)
	if err != nil {
		logger.Log.Warnf("[goal-pursuit] failed to query goals: %v", err)
		return
	}
	defer goalRows.Close()

	// Pick the first goal not already attempted.
	var selected *activeGoal
	for goalRows.Next() {
		var g activeGoal
		if err := goalRows.Scan(&g.ID, &g.Summary); err != nil {
			continue
		}
		if isGoalAlreadyAttempted(ctx, e.DB, g.Summary) {
			continue
		}
		selected = &g
		break
	}

	if selected == nil {
		return
	}

	// Launch background task via CuriosityFunc at normal priority (5).
	directive := fmt.Sprintf("[goal] %s", cleanGoalSummary(selected.Summary))
	taskID, err := e.Cognitive.CuriosityFunc(directive, 5)
	if err != nil {
		logger.Log.Warnf("[goal-pursuit] failed to launch: %v", err)
		return
	}

	e.lastGoalPursuitRun = time.Now()
	logger.Log.Infof("[goal-pursuit] launched task #%d: %s", taskID, cleanGoalSummary(selected.Summary))
}
