package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/textutil"
)

// planAndExecuteArgs are the arguments parsed from the orchestrator's tool call.
type planAndExecuteArgs struct {
	Directive  string `json:"directive"`
	Context    string `json:"context"`
	Background bool   `json:"background"`
	Priority   int    `json:"priority"`
}

// planStep represents a single decomposed step from DTC.
type planStep struct {
	Description string   `json:"description"`
	ToolsNeeded []string `json:"tools_needed"`
}

// taskPlan is the structured output from DTC decomposition.
type taskPlan struct {
	Steps []planStep `json:"steps"`
}

// stepResult records the outcome of executing a single step.
type stepResult struct {
	Step        int
	Description string
	Result      string
	Success     bool
}

// BackgroundTaskRunner manages async task execution with DB-backed state.
type BackgroundTaskRunner struct {
	db       *pgxpool.Pool
	sendFunc func(string)
	mu       sync.Mutex
	running  map[int64]context.CancelFunc
	sem      chan struct{} // concurrency limiter (cap 3)
}

// NewBackgroundTaskRunner creates a runner with the given DB pool and
// Telegram send function.
func NewBackgroundTaskRunner(db *pgxpool.Pool, sendFunc func(string)) *BackgroundTaskRunner {
	return &BackgroundTaskRunner{
		db:       db,
		sendFunc: sendFunc,
		running:  make(map[int64]context.CancelFunc),
		sem:      make(chan struct{}, 3),
	}
}

// decomposePlan calls DTC to break a directive into concrete steps.
func decomposePlan(ctx context.Context, dtc *DeepThinkerClient, directive, extraContext string) (*taskPlan, error) {
	userContent := directive
	if extraContext != "" {
		userContent = fmt.Sprintf("%s\n\nContext:\n%s", directive, extraContext)
	}

	decompCtx, cancel := context.WithTimeout(ctx, TimeoutPlanDecomposition)
	defer cancel()

	raw, err := dtc.CompleteNoThink(decompCtx, strings.TrimSpace(prompts.PlanTask), userContent, 2048)
	if err != nil {
		return nil, fmt.Errorf("plan decomposition: %w", err)
	}

	raw = textutil.CleanLLMJSON(raw)

	var plan taskPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return nil, fmt.Errorf("parse plan: %w (raw: %s)", err, raw)
	}

	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("plan produced zero steps")
	}
	const maxPlanSteps = 6
	if len(plan.Steps) > maxPlanSteps {
		plan.Steps = plan.Steps[:maxPlanSteps]
		logger.Log.Warnf("[plan] truncated plan to %d steps", maxPlanSteps)
	}

	return &plan, nil
}

// executeSteps runs each plan step sequentially through SubagentSupervisor.
// Each step receives accumulated context from prior steps. Step failures are
// recorded but do not abort the remaining steps.
func executeSteps(ctx context.Context, sc *SubagentClient, dc *DelegateConfig,
	registry *Registry, directive string, steps []planStep,
	progressFn func(completed, total int)) []stepResult {

	results := make([]stepResult, 0, len(steps))

	for i, step := range steps {
		select {
		case <-ctx.Done():
			results = append(results, stepResult{
				Step:        i + 1,
				Description: step.Description,
				Result:      "cancelled: " + ctx.Err().Error(),
				Success:     false,
			})
			return results
		default:
		}

		systemPrompt := buildStepSystemPrompt(directive, step, results)

		toolExec := NewScopedToolExec(registry, dc)

		stepCtx, stepCancel := context.WithTimeout(ctx, TimeoutPlanStepExecution)

		logger.Log.Infof("[plan] executing step %d/%d: %s", i+1, len(steps), step.Description)
		result, err := SubagentSupervisor(stepCtx, sc, dc.Grammar(), systemPrompt,
			step.Description, toolExec, 10)
		stepCancel()

		sr := stepResult{
			Step:        i + 1,
			Description: step.Description,
			Success:     err == nil,
		}
		if err != nil {
			sr.Result = fmt.Sprintf("step failed: %v", err)
			logger.Log.Warnf("[plan] step %d failed: %v", i+1, err)
		} else {
			sr.Result = result
			logger.Log.Infof("[plan] step %d completed", i+1)
		}
		results = append(results, sr)

		if progressFn != nil {
			progressFn(i+1, len(steps))
		}
	}

	return results
}

// buildStepSystemPrompt constructs the system prompt for a single step's
// SubagentSupervisor call, including the overall goal and accumulated context.
func buildStepSystemPrompt(directive string, step planStep, priorResults []stepResult) string {
	var b strings.Builder
	b.WriteString("You are executing one step of a multi-step plan.\n\n")
	fmt.Fprintf(&b, "## Overall Goal\n%s\n\n", directive)
	fmt.Fprintf(&b, "## Your Current Step\n%s\n\n", step.Description)

	if len(priorResults) > 0 {
		b.WriteString("## Results from Prior Steps\n")
		for _, r := range priorResults {
			status := "SUCCESS"
			if !r.Success {
				status = "FAILED"
			}
			fmt.Fprintf(&b, "Step %d [%s]: %s\nResult: %s\n\n", r.Step, status, r.Description, r.Result)
		}
	}

	b.WriteString("## Rules\n")
	b.WriteString("- Execute your assigned step using the available tools.\n")
	b.WriteString("- Build upon results from prior steps when relevant.\n")
	b.WriteString("- Be concise and factual in your response.\n")
	b.WriteString("- When you have completed the step, respond with your findings.\n")

	return b.String()
}

// formatResults formats step results into a human-readable summary.
func formatResults(results []stepResult) string {
	var b strings.Builder
	succeeded := 0
	for _, r := range results {
		if r.Success {
			succeeded++
		}
	}
	fmt.Fprintf(&b, "Plan completed: %d/%d steps succeeded.\n\n", succeeded, len(results))

	for _, r := range results {
		status := "OK"
		if !r.Success {
			status = "FAILED"
		}
		fmt.Fprintf(&b, "**Step %d** [%s]: %s\n%s\n\n", r.Step, status, r.Description, r.Result)
	}
	return b.String()
}

// --- BackgroundTaskRunner methods ---

// Start creates a DB row, launches a goroutine, and returns the task ID.
// Planning must complete before calling this (pass pre-decomposed steps).
func (btr *BackgroundTaskRunner) Start(directive string, priority int, steps []planStep,
	sc *SubagentClient, dc *DelegateConfig, registry *Registry) (int64, error) {

	ctx := context.Background()
	var taskID int64
	err := btr.db.QueryRow(ctx,
		`INSERT INTO background_tasks (directive, status, steps_total, priority) VALUES ($1, 'running', $2, $3) RETURNING id`,
		directive, len(steps), priority,
	).Scan(&taskID)
	if err != nil {
		return 0, fmt.Errorf("create background task: %w", err)
	}

	bgCtx, cancel := context.WithTimeout(ctx, TimeoutPlanBackground)

	btr.mu.Lock()
	btr.running[taskID] = cancel
	btr.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			btr.mu.Lock()
			delete(btr.running, taskID)
			btr.mu.Unlock()
		}()

		// Acquire concurrency slot.
		select {
		case btr.sem <- struct{}{}:
		case <-bgCtx.Done():
			btr.updateTask(taskID, "failed", "", "timed out waiting for execution slot")
			return
		}
		defer func() { <-btr.sem }()

		progressFn := func(completed, total int) {
			if _, err := btr.db.Exec(context.Background(),
				`UPDATE background_tasks SET steps_completed = $1 WHERE id = $2`,
				completed, taskID,
			); err != nil {
				logger.Log.Warnf("[background] failed to update progress for task %d: %v", taskID, err)
			}
		}

		results := executeSteps(bgCtx, sc, dc, registry, directive, steps, progressFn)
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

		btr.updateTask(taskID, status, formatted, errMsg)

		if btr.sendFunc != nil {
			label := "completed"
			if status == "failed" {
				label = "failed"
			}
			notification := fmt.Sprintf("Background task #%d %s: %s\n\n%d/%d steps succeeded.",
				taskID, label, directive, succeeded, len(results))
			btr.sendFunc(textutil.Truncate(notification, 500))
		}
	}()

	return taskID, nil
}

func (btr *BackgroundTaskRunner) updateTask(id int64, status, result, errMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanProgressDB)
	defer cancel()
	_, err := btr.db.Exec(ctx,
		`UPDATE background_tasks SET status = $1, result = $2, error_message = $3, completed_at = now() WHERE id = $4`,
		status, result, errMsg, id,
	)
	if err != nil {
		logger.Log.Errorf("[background] failed to update task %d: %v", id, err)
	}
}

// Status returns the current state of a background task.
func (btr *BackgroundTaskRunner) Status(ctx context.Context, taskID int64) (string, error) {
	var status, directive string
	var result, errMsg *string
	var stepsTotal, stepsCompleted, priority int
	var createdAt time.Time
	var completedAt *time.Time

	err := btr.db.QueryRow(ctx,
		`SELECT directive, status, result, error_message, steps_total, steps_completed, created_at, completed_at, COALESCE(priority, 5)
		 FROM background_tasks WHERE id = $1`, taskID,
	).Scan(&directive, &status, &result, &errMsg, &stepsTotal, &stepsCompleted, &createdAt, &completedAt, &priority)
	if err != nil {
		return "", fmt.Errorf("task not found: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Task #%d: %s\n", taskID, directive)
	fmt.Fprintf(&b, "Status: %s | Priority: %d\n", status, priority)
	fmt.Fprintf(&b, "Progress: %d/%d steps\n", stepsCompleted, stepsTotal)
	fmt.Fprintf(&b, "Started: %s\n", createdAt.Format(time.RFC3339))
	if completedAt != nil {
		fmt.Fprintf(&b, "Completed: %s\n", completedAt.Format(time.RFC3339))
	}
	if errMsg != nil && *errMsg != "" {
		fmt.Fprintf(&b, "Error: %s\n", *errMsg)
	}
	if result != nil && *result != "" {
		fmt.Fprintf(&b, "\nResults:\n%s", *result)
	}
	return b.String(), nil
}

// Cancel cancels a running background task.
func (btr *BackgroundTaskRunner) Cancel(taskID int64) (string, error) {
	btr.mu.Lock()
	cancelFn, exists := btr.running[taskID]
	btr.mu.Unlock()

	if !exists {
		return fmt.Sprintf("Task #%d is not currently running.", taskID), nil
	}

	cancelFn()
	btr.updateTask(taskID, "cancelled", "", "cancelled by user")

	btr.mu.Lock()
	delete(btr.running, taskID)
	btr.mu.Unlock()

	return fmt.Sprintf("Task #%d cancelled.", taskID), nil
}

// CleanupOrphans marks any 'running' tasks in the DB as 'failed' on startup.
func (btr *BackgroundTaskRunner) CleanupOrphans() {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanProgressDB)
	defer cancel()
	result, err := btr.db.Exec(ctx,
		`UPDATE background_tasks SET status = 'failed', error_message = 'orphaned: process restarted', completed_at = now()
		 WHERE status = 'running'`)
	if err != nil {
		logger.Log.Errorf("[background] orphan cleanup failed: %v", err)
		return
	}
	if result.RowsAffected() > 0 {
		logger.Log.Infof("[background] cleaned up %d orphaned tasks", result.RowsAffected())
	}
}

// CleanupOldTasks deletes completed/failed/cancelled tasks older than 7 days.
func (btr *BackgroundTaskRunner) CleanupOldTasks() {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutPlanProgressDB)
	defer cancel()
	result, err := btr.db.Exec(ctx,
		`DELETE FROM background_tasks
		 WHERE status IN ('completed', 'failed', 'cancelled')
		   AND completed_at < NOW() - INTERVAL '7 days'`)
	if err != nil {
		logger.Log.Errorf("[background] old task cleanup failed: %v", err)
		return
	}
	if result.RowsAffected() > 0 {
		logger.Log.Infof("[background] cleaned up %d old tasks", result.RowsAffected())
	}
}

// List returns a summary of running and recently completed background tasks.
func (btr *BackgroundTaskRunner) List(ctx context.Context) (string, error) {
	rows, err := btr.db.Query(ctx,
		`SELECT id, directive, status, COALESCE(priority, 5), steps_total, steps_completed
		 FROM background_tasks
		 WHERE status = 'running'
		    OR (status IN ('completed', 'failed', 'cancelled') AND completed_at >= NOW() - INTERVAL '24 hours')
		 ORDER BY
		    CASE WHEN status = 'running' THEN 0 ELSE 1 END,
		    priority DESC,
		    created_at DESC
		 LIMIT 10`)
	if err != nil {
		return "", fmt.Errorf("list background tasks: %w", err)
	}
	defer rows.Close()

	var b strings.Builder
	b.WriteString("Background Tasks:\n")
	b.WriteString("ID  | Status     | Pri | Progress | Directive\n")
	b.WriteString("----|------------|-----|----------|----------\n")
	count := 0
	for rows.Next() {
		var id int64
		var directive, status string
		var priority, stepsTotal, stepsCompleted int
		if err := rows.Scan(&id, &directive, &status, &priority, &stepsTotal, &stepsCompleted); err != nil {
			continue
		}
		count++
		dir := textutil.Truncate(directive, 37)
		fmt.Fprintf(&b, "%-4d| %-10s | %-3d | %d/%-6d | %s\n", id, status, priority, stepsCompleted, stepsTotal, dir)
	}
	if count == 0 {
		b.WriteString("(no tasks)\n")
	}
	return b.String(), nil
}

// --- Tool constructors ---

// NewPlanAndExecute returns a ToolFunc that decomposes a directive into steps
// via DTC, then executes them via SubagentSupervisor with accumulated context.
// When background=true, planning runs synchronously but execution is async.
func NewPlanAndExecute(dtc *DeepThinkerClient, sc *SubagentClient,
	dc *DelegateConfig, registry *Registry, btr *BackgroundTaskRunner) ToolFunc {

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a planAndExecuteArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if strings.TrimSpace(a.Directive) == "" {
			return "directive is required", nil
		}
		if a.Priority < 1 || a.Priority > 10 {
			a.Priority = 5
		}

		extraContext := a.Context
		if len(extraContext) > maxDelegateContextLen {
			extraContext = extraContext[:maxDelegateContextLen] + "\n... (truncated)"
		}

		// Phase 1: Decompose (always synchronous).
		plan, err := decomposePlan(ctx, dtc, a.Directive, extraContext)
		if err != nil {
			return fmt.Sprintf("Failed to decompose plan: %v", err), nil
		}

		logger.Log.Infof("[plan] decomposed into %d steps for: %s", len(plan.Steps), a.Directive)
		for i, s := range plan.Steps {
			logger.Log.Infof("[plan]   step %d: %s (tools: %v)", i+1, s.Description, s.ToolsNeeded)
		}

		// Phase 2: Execute.
		if a.Background && btr != nil {
			taskID, err := btr.Start(a.Directive, a.Priority, plan.Steps, sc, dc, registry)
			if err != nil {
				return fmt.Sprintf("Failed to start background task: %v", err), nil
			}
			return fmt.Sprintf("Background task #%d started with %d steps. Use check_background_task to monitor progress.", taskID, len(plan.Steps)), nil
		}

		// Foreground mode.
		fgCtx, cancel := context.WithTimeout(ctx, TimeoutPlanForeground)
		defer cancel()

		results := executeSteps(fgCtx, sc, dc, registry, a.Directive, plan.Steps, nil)
		return formatResults(results), nil
	}
}

// NewCheckBackgroundTask returns a ToolFunc for listing, checking status, or
// cancelling background tasks.
func NewCheckBackgroundTask(btr *BackgroundTaskRunner) ToolFunc {
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
			return btr.List(ctx)
		case "status":
			if a.TaskID <= 0 {
				return "task_id is required for status action", nil
			}
			return btr.Status(ctx, a.TaskID)
		case "cancel":
			if a.TaskID <= 0 {
				return "task_id is required for cancel action", nil
			}
			return btr.Cancel(a.TaskID)
		default:
			return fmt.Sprintf("unknown action %q (use 'list', 'status', or 'cancel')", action), nil
		}
	}
}
