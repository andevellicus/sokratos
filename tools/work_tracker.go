package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	objpkg "sokratos/objectives"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/textutil"
)

// ObjectiveTaskResult captures the outcome of a background task linked to an objective.
type ObjectiveTaskResult struct {
	ObjectiveID int64
	Directive   string
	Status      string // "completed" or "failed"
	Result      string // formatted step results
}

// WorkTracker manages async task execution with DB-backed state. It tracks
// background plans, routine executions, and scheduled tasks in the unified
// work_items table. Cancel funcs for all running work are held in memory
// for watchdog-based killing of hung work.
type WorkTracker struct {
	db         *pgxpool.Pool
	sendFunc   func(string)
	OnComplete              func(directive, status string)     // called after a background task finishes (nil = no-op)
	OnObjectiveTaskComplete func(ObjectiveTaskResult)          // called when an objective-linked task finishes (nil = no-op)
	ShareGate               func(directive, result string)     // quality gate for proactive sharing (nil = disabled)
	mu         sync.Mutex
	active     map[int64]context.CancelFunc // cancel funcs for all running work
	sem        chan struct{}                 // concurrency limiter for background plans (cap 3)
}

// NewWorkTracker creates a tracker with the given DB pool and Telegram send function.
func NewWorkTracker(db *pgxpool.Pool, sendFunc func(string)) *WorkTracker {
	return &WorkTracker{
		db:       db,
		sendFunc: sendFunc,
		active:   make(map[int64]context.CancelFunc),
		sem:      make(chan struct{}, 3),
	}
}

// Start creates a DB row for a background plan, launches a goroutine, and
// returns the task ID. Planning must complete before calling this.
// objectiveID links the task to an objective (0 = no objective).
func (wt *WorkTracker) Start(directive string, priority int, objectiveID int64, steps []planStep,
	deps PlanExecDeps) (int64, error) {

	ctx := context.Background()

	// Use NULLIF to set objective_id only when > 0 (single query, nullable param).
	var taskID int64
	err := wt.db.QueryRow(ctx,
		`INSERT INTO work_items (type, directive, status, steps_total, priority, started_at, timeout_at, objective_id)
		 VALUES ('background', $1, 'running', $2, $3, now(), now() + $4::interval, NULLIF($5, 0))
		 RETURNING id`,
		directive, len(steps), priority, fmt.Sprintf("%d seconds", int(TimeoutPlanBackground.Seconds())), objectiveID,
	).Scan(&taskID)
	if err != nil {
		return 0, fmt.Errorf("create background task: %w", err)
	}

	// Update objective status when linked.
	if objectiveID > 0 {
		objpkg.UpdateStatus(ctx, wt.db, objectiveID, "in_progress")
		objpkg.IncrementAttempts(ctx, wt.db, objectiveID)
	}

	bgCtx, cancel := context.WithTimeout(ctx, TimeoutPlanBackground)

	wt.mu.Lock()
	wt.active[taskID] = cancel
	wt.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			wt.mu.Lock()
			delete(wt.active, taskID)
			wt.mu.Unlock()
		}()

		// Acquire concurrency slot.
		select {
		case wt.sem <- struct{}{}:
		case <-bgCtx.Done():
			wt.updateWork(taskID, "failed", "", "timed out waiting for execution slot")
			return
		}
		defer func() { <-wt.sem }()

		progressFn := func(completed, total int) {
			if _, err := wt.db.Exec(context.Background(),
				`UPDATE work_items SET steps_completed = $1 WHERE id = $2`,
				completed, taskID,
			); err != nil {
				logger.Log.Warnf("[work-tracker] failed to update progress for task %d: %v", taskID, err)
			}
		}

		results := executeSteps(bgCtx, deps, directive, steps, progressFn)
		formatted := formatResults(results)

		succeeded := 0
		for _, r := range results {
			if r.Success {
				succeeded++
			}
		}

		status := "completed"
		var errMsg string
		if succeeded == 0 {
			status = "failed"
			errMsg = "all steps failed"
		} else if succeeded < len(results) {
			errMsg = fmt.Sprintf("%d/%d steps failed", len(results)-succeeded, len(results))
		}

		wt.updateWork(taskID, status, formatted, errMsg)

		if wt.sendFunc != nil {
			label := "completed"
			if status == "failed" {
				label = "failed"
			}
			notification := fmt.Sprintf("Background task #%d %s: %s\n\n%d/%d steps succeeded.",
				taskID, label, directive, succeeded, len(results))
			wt.sendFunc(textutil.Truncate(notification, 500))
		}

		if wt.OnComplete != nil {
			wt.OnComplete(directive, status)
		}

		// Fire objective-linked callback for initiative chaining.
		if objectiveID > 0 && wt.OnObjectiveTaskComplete != nil {
			wt.OnObjectiveTaskComplete(ObjectiveTaskResult{
				ObjectiveID: objectiveID,
				Directive:   directive,
				Status:      status,
				Result:      formatted,
			})
		}

		// Fire proactive sharing gate.
		if wt.ShareGate != nil && succeeded > 0 {
			wt.ShareGate(directive, formatted)
		}
	}()

	return taskID, nil
}

// TrackStart inserts a running work_items row and returns its ID. The caller
// is responsible for calling SetCancel and TrackEnd. Used by routine and
// scheduled task execution to make their work visible to the watchdog.
func (wt *WorkTracker) TrackStart(workType, directive string, timeout time.Duration) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanProgressDB)
	defer cancel()

	var id int64
	err := wt.db.QueryRow(ctx,
		`INSERT INTO work_items (type, directive, status, started_at, timeout_at)
		 VALUES ($1, $2, 'running', now(), now() + $3::interval)
		 RETURNING id`,
		workType, directive, fmt.Sprintf("%d seconds", int(timeout.Seconds())),
	).Scan(&id)
	if err != nil {
		logger.Log.Warnf("[work-tracker] failed to track start (%s %q): %v", workType, directive, err)
		return 0
	}
	return id
}

// SetCancel stores a cancel func for a tracked work item, enabling the
// watchdog to kill it if it hangs past timeout_at.
func (wt *WorkTracker) SetCancel(id int64, cancelFn context.CancelFunc) {
	if id == 0 {
		return
	}
	wt.mu.Lock()
	wt.active[id] = cancelFn
	wt.mu.Unlock()
}

// TrackEnd marks a tracked work item as completed/failed and removes its
// cancel func from the active map.
func (wt *WorkTracker) TrackEnd(id int64, status, errMsg string) {
	if id == 0 {
		return
	}
	wt.mu.Lock()
	delete(wt.active, id)
	wt.mu.Unlock()

	wt.updateWork(id, status, "", errMsg)
}

// KillHungWork queries work_items past their timeout_at, cancels their
// contexts, marks them as failed, and logs to failed_operations. Returns
// the number of items killed.
func (wt *WorkTracker) KillHungWork() int {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanProgressDB)
	defer cancel()

	rows, err := wt.db.Query(ctx,
		`SELECT id, type, directive FROM work_items
		 WHERE status = 'running' AND timeout_at IS NOT NULL AND timeout_at < now()`)
	if err != nil {
		logger.Log.Warnf("[work-tracker] hung work query failed: %v", err)
		return 0
	}
	defer rows.Close()

	var killed int
	for rows.Next() {
		var id int64
		var workType, directive string
		if err := rows.Scan(&id, &workType, &directive); err != nil {
			continue
		}

		// Cancel the context if we have it.
		wt.mu.Lock()
		if cancelFn, ok := wt.active[id]; ok {
			cancelFn()
			delete(wt.active, id)
		}
		wt.mu.Unlock()

		wt.updateWork(id, "failed", "", "killed by watchdog: exceeded timeout")

		memory.LogFailedOp(wt.db, "watchdog_kill", fmt.Sprintf("%s #%d: %s", workType, id, directive),
			fmt.Errorf("work item exceeded timeout"), map[string]any{
				"work_id":   id,
				"work_type": workType,
				"directive": directive,
			})

		logger.Log.Warnf("[work-tracker] killed hung %s #%d: %s", workType, id, directive)
		killed++
	}
	return killed
}

func (wt *WorkTracker) updateWork(id int64, status, result, errMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanProgressDB)
	defer cancel()
	_, err := wt.db.Exec(ctx,
		`UPDATE work_items SET status = $1, result = $2, error_message = $3, completed_at = now() WHERE id = $4`,
		status, result, errMsg, id,
	)
	if err != nil {
		logger.Log.Errorf("[work-tracker] failed to update work item %d: %v", id, err)
	}
}

// Status returns the current state of a work item.
func (wt *WorkTracker) Status(ctx context.Context, taskID int64) (string, error) {
	var status, directive, workType string
	var result, errMsg *string
	var stepsTotal, stepsCompleted, priority int
	var createdAt time.Time
	var completedAt, startedAt, timeoutAt *time.Time

	err := wt.db.QueryRow(ctx,
		`SELECT type, directive, status, result, error_message,
		        steps_total, steps_completed, created_at, completed_at,
		        COALESCE(priority, 5), started_at, timeout_at
		 FROM work_items WHERE id = $1`, taskID,
	).Scan(&workType, &directive, &status, &result, &errMsg,
		&stepsTotal, &stepsCompleted, &createdAt, &completedAt,
		&priority, &startedAt, &timeoutAt)
	if err != nil {
		return "", fmt.Errorf("work item not found: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Work Item #%d [%s]: %s\n", taskID, workType, directive)
	fmt.Fprintf(&b, "Status: %s | Priority: %d\n", status, priority)
	if stepsTotal > 0 {
		fmt.Fprintf(&b, "Progress: %d/%d steps\n", stepsCompleted, stepsTotal)
	}
	fmt.Fprintf(&b, "Created: %s\n", createdAt.Format(time.RFC3339))
	if startedAt != nil {
		fmt.Fprintf(&b, "Started: %s\n", startedAt.Format(time.RFC3339))
	}
	if completedAt != nil {
		fmt.Fprintf(&b, "Completed: %s\n", completedAt.Format(time.RFC3339))
	}
	if timeoutAt != nil && status == "running" {
		fmt.Fprintf(&b, "Timeout: %s\n", timeoutAt.Format(time.RFC3339))
	}
	if errMsg != nil && *errMsg != "" {
		fmt.Fprintf(&b, "Error: %s\n", *errMsg)
	}
	if result != nil && *result != "" {
		fmt.Fprintf(&b, "\nResults:\n%s", *result)
	}
	return b.String(), nil
}

// Cancel cancels a running work item.
func (wt *WorkTracker) Cancel(taskID int64) (string, error) {
	wt.mu.Lock()
	cancelFn, exists := wt.active[taskID]
	wt.mu.Unlock()

	if !exists {
		return fmt.Sprintf("Work item #%d is not currently running.", taskID), nil
	}

	cancelFn()
	wt.updateWork(taskID, "cancelled", "", "cancelled by user")

	wt.mu.Lock()
	delete(wt.active, taskID)
	wt.mu.Unlock()

	return fmt.Sprintf("Work item #%d cancelled.", taskID), nil
}

// CleanupOrphans marks any 'running' work items in the DB as 'failed' on startup.
func (wt *WorkTracker) CleanupOrphans() {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanProgressDB)
	defer cancel()
	result, err := wt.db.Exec(ctx,
		`UPDATE work_items SET status = 'failed', error_message = 'orphaned: process restarted', completed_at = now()
		 WHERE status = 'running'`)
	if err != nil {
		logger.Log.Errorf("[work-tracker] orphan cleanup failed: %v", err)
		return
	}
	if result.RowsAffected() > 0 {
		logger.Log.Infof("[work-tracker] cleaned up %d orphaned work items", result.RowsAffected())
	}
}

// CleanupOldTasks deletes completed/failed/cancelled work items older than 7 days.
func (wt *WorkTracker) CleanupOldTasks() {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanProgressDB)
	defer cancel()
	result, err := wt.db.Exec(ctx,
		`DELETE FROM work_items
		 WHERE status IN ('completed', 'failed', 'cancelled')
		   AND completed_at < NOW() - INTERVAL '7 days'`)
	if err != nil {
		logger.Log.Errorf("[work-tracker] old task cleanup failed: %v", err)
		return
	}
	if result.RowsAffected() > 0 {
		logger.Log.Infof("[work-tracker] cleaned up %d old work items", result.RowsAffected())
	}
}

// List returns a summary of running and recently completed work items (all types).
func (wt *WorkTracker) List(ctx context.Context) (string, error) {
	rows, err := wt.db.Query(ctx,
		`SELECT id, type, directive, status, COALESCE(priority, 5), steps_total, steps_completed
		 FROM work_items
		 WHERE status = 'running'
		    OR (status IN ('completed', 'failed', 'cancelled') AND completed_at >= NOW() - INTERVAL '24 hours')
		 ORDER BY
		    CASE WHEN status = 'running' THEN 0 ELSE 1 END,
		    priority DESC,
		    created_at DESC
		 LIMIT 10`)
	if err != nil {
		return "", fmt.Errorf("list work items: %w", err)
	}
	defer rows.Close()

	var b strings.Builder
	b.WriteString("Work Items:\n")
	b.WriteString("ID  | Type       | Status     | Pri | Progress | Directive\n")
	b.WriteString("----|------------|------------|-----|----------|----------\n")
	count := 0
	for rows.Next() {
		var id int64
		var workType, directive, status string
		var priority, stepsTotal, stepsCompleted int
		if err := rows.Scan(&id, &workType, &directive, &status, &priority, &stepsTotal, &stepsCompleted); err != nil {
			continue
		}
		count++
		dir := textutil.Truncate(directive, 30)
		fmt.Fprintf(&b, "%-4d| %-10s | %-10s | %-3d | %d/%-6d | %s\n", id, workType, status, priority, stepsCompleted, stepsTotal, dir)
	}
	if count == 0 {
		b.WriteString("(no work items)\n")
	}
	return b.String(), nil
}

// LaunchBackgroundPlan decomposes a directive via DTC and launches it as a
// background work item. Returns the task ID. Used by the curiosity engine.
// objectiveID links the task to an objective (0 = no objective).
func LaunchBackgroundPlan(wt *WorkTracker, deps PlanExecDeps,
	directive string, priority int, objectiveID int64) (int64, error) {

	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanDecomposition)
	defer cancel()

	plan, err := decomposePlan(ctx, deps.DTC, directive, "")
	if err != nil {
		return 0, fmt.Errorf("curiosity plan decomposition: %w", err)
	}
	return wt.Start(directive, priority, objectiveID, plan.Steps, deps)
}

// NewCheckBackgroundTask returns a ToolFunc for listing, checking status, or
// cancelling work items.
func NewCheckBackgroundTask(wt *WorkTracker) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			TaskID int64  `json:"task_id"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}

		action := a.Action
		if action == "" {
			action = "status"
		}

		switch action {
		case "list":
			return wt.List(ctx)
		case "status":
			if a.TaskID <= 0 {
				return "task_id is required for status action", nil
			}
			return wt.Status(ctx, a.TaskID)
		case "cancel":
			if a.TaskID <= 0 {
				return "task_id is required for cancel action", nil
			}
			return wt.Cancel(a.TaskID)
		default:
			return fmt.Sprintf("unknown action %q (use 'list', 'status', or 'cancel')", action), nil
		}
	}
}
