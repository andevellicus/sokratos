package engine

import (
	"context"
	"fmt"
	"time"

	"sokratos/logger"
	"sokratos/objectives"
	"sokratos/timeouts"
)

const (
	defaultObjectivePursuitCooldown = 4 * time.Hour
	objectivePursuitMaxInactive     = 24 * time.Hour
)

// runObjectivePursuitIfReady selects the highest-priority active objective that hasn't
// been pursued recently and launches a background task for it.
func (e *Engine) runObjectivePursuitIfReady() {
	if e.CogServices == nil || e.DB == nil {
		return
	}

	// Cooldown check.
	cooldown := e.ObjectivePursuitCooldown
	if cooldown == 0 {
		cooldown = defaultObjectivePursuitCooldown
	}
	if time.Since(e.lastObjectivePursuitRun) < cooldown {
		return
	}

	// User activity check — only pursue objectives when the user was recently active.
	lastActivity := e.SM.LastUserActivity()
	if lastActivity.IsZero() || time.Since(lastActivity) > objectivePursuitMaxInactive {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()

	// Check for already-running objective task.
	var running int
	if err := e.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_items
		 WHERE status = 'running' AND directive LIKE '[objective]%'`).Scan(&running); err == nil && running > 0 {
		logger.Log.Debug("[objective-pursuit] task already running, skipping")
		return
	}

	// Query active objectives from the objectives table.
	active, err := objectives.ListActive(ctx, e.DB)
	if err != nil || len(active) == 0 {
		return
	}

	// Pick the first objective not pursued within the cooldown window.
	var selected *objectives.Objective
	for i := range active {
		g := &active[i]
		if g.LastPursued != nil && time.Since(*g.LastPursued) < cooldown {
			continue
		}
		selected = g
		break
	}

	if selected == nil {
		return
	}

	// If the objective is stuck (3+ attempts without completion), emit a
	// curiosity signal for alternative approaches instead of retrying the same way.
	if selected.Attempts >= 3 && e.CuriositySignals != nil {
		select {
		case e.CuriositySignals <- CuriositySignal{
			Source:      "objective",
			Query:       fmt.Sprintf("alternative approaches to: %s", selected.Summary),
			Priority:    7,
			ObjectiveID: selected.ID,
		}:
			logger.Log.Infof("[objective-pursuit] objective #%d stuck after %d attempts, emitted curiosity signal", selected.ID, selected.Attempts)
		default:
		}
		e.lastObjectivePursuitRun = time.Now()
		return
	}

	// Launch background task via CogServices at normal priority (5).
	directive := fmt.Sprintf("[objective] %s", selected.Summary)
	taskID, err := e.CogServices.LaunchCuriosity(directive, 5, selected.ID)
	if err != nil {
		logger.Log.Warnf("[objective-pursuit] failed to launch: %v", err)
		return
	}

	e.lastObjectivePursuitRun = time.Now()
	logger.Log.Infof("[objective-pursuit] launched task #%d for objective #%d: %s", taskID, selected.ID, selected.Summary)
}
