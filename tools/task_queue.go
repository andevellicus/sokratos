package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type addTaskArgs struct {
	Task  string `json:"task"`
	DueAt string `json:"due_at,omitempty"` // RFC3339
	Recur string `json:"recur,omitempty"`  // Go duration, e.g. "24h", "1h", "168h"
}

// NewAddTask returns a ToolFunc that inserts a task into the PostgreSQL tasks
// table. After inserting, it sends a signal on interruptChan to wake the
// scheduler goroutine in case the new task is due sooner than the current wait.
func NewAddTask(pool *pgxpool.Pool, interruptChan chan struct{}) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a addTaskArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Task == "" {
			return "error: task is required", nil
		}

		var dueAt *time.Time
		var recurrenceNs int64

		if a.DueAt != "" {
			t, err := time.Parse(time.RFC3339, a.DueAt)
			if err != nil {
				return fmt.Sprintf("invalid due_at (expected RFC3339): %v", err), nil
			}
			dueAt = &t
		}

		if a.Recur != "" {
			d, err := time.ParseDuration(a.Recur)
			if err != nil {
				return fmt.Sprintf("invalid recur (expected Go duration like \"24h\"): %v", err), nil
			}
			recurrenceNs = int64(d)
			// If no due_at set, first occurrence is now + recur.
			if dueAt == nil {
				t := time.Now().Add(d)
				dueAt = &t
			}
		}

		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO tasks (description, due_at, recurrence, status) VALUES ($1, $2, $3, 'pending') RETURNING id`,
			a.Task, dueAt, recurrenceNs).Scan(&id)
		if err != nil {
			return fmt.Sprintf("failed to insert task: %v", err), nil
		}

		// Wake the scheduler in case this task is due sooner than the current wait.
		select {
		case interruptChan <- struct{}{}:
		default:
		}

		desc := a.Task
		if dueAt != nil {
			desc += fmt.Sprintf(" (due: %s)", dueAt.Format(time.RFC3339))
		}
		if recurrenceNs > 0 {
			desc += fmt.Sprintf(" (every %s)", time.Duration(recurrenceNs))
		}
		return fmt.Sprintf("Task added (id: %d): %s", id, desc), nil
	}
}

type completeTaskArgs struct {
	TaskID int64 `json:"task_id,omitempty"`
}

// NewCompleteTask returns a ToolFunc that marks a task as completed in
// PostgreSQL. If no task_id is provided, it completes the oldest pending task.
func NewCompleteTask(pool *pgxpool.Pool, interruptChan chan struct{}) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a completeTaskArgs
		if len(args) > 0 {
			_ = json.Unmarshal(args, &a)
		}

		var id int64
		var desc string

		if a.TaskID > 0 {
			err := pool.QueryRow(ctx,
				`UPDATE tasks SET status = 'completed' WHERE id = $1 AND status = 'pending' RETURNING id, description`,
				a.TaskID).Scan(&id, &desc)
			if err != nil {
				return fmt.Sprintf("No pending task with id %d found.", a.TaskID), nil
			}
		} else {
			err := pool.QueryRow(ctx,
				`UPDATE tasks SET status = 'completed'
				 WHERE id = (SELECT id FROM tasks WHERE status = 'pending' ORDER BY due_at ASC NULLS LAST LIMIT 1)
				 RETURNING id, description`).Scan(&id, &desc)
			if err != nil {
				return "No pending tasks to complete.", nil
			}
		}

		// Wake the scheduler to recalculate the next pending task.
		select {
		case interruptChan <- struct{}{}:
		default:
		}

		return fmt.Sprintf("Completed task #%d: %s", id, desc), nil
	}
}
