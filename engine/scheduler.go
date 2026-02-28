package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/timefmt"
	"sokratos/timeouts"
)

// runTaskScheduler is a long-running goroutine that queries the database for
// the next pending scheduled task and waits until it's due before executing it.
// It uses a select block to handle both timer expiry and interrupt signals from
// new task insertions or completions.
func (e *Engine) runTaskScheduler() {
	logger.Log.Info("[scheduler] task scheduler started")
	for {
		task, err := e.fetchNextPendingTask()
		if err != nil {
			logger.Log.Errorf("[scheduler] query error: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}

		if task == nil {
			// No pending scheduled tasks; block until one is added.
			<-e.InterruptChan
			continue
		}

		delay := time.Until(*task.DueAt)
		if delay <= 0 {
			// Task is already past due — execute immediately.
			e.executeTask(*task)
			continue
		}

		logger.Log.Infof("[scheduler] next task %q (#%d) due in %s", task.Description, task.ID, delay)
		timer := time.NewTimer(delay)

		select {
		case <-timer.C:
			e.executeTask(*task)
		case <-e.InterruptChan:
			timer.Stop()
			// Recalculate — a new task may be due sooner.
		}
	}
}

// fetchNextPendingTask returns the earliest pending task with a due_at, or nil
// if no scheduled tasks are pending.
func (e *Engine) fetchNextPendingTask() (*Task, error) {
	row := e.DB.QueryRow(context.Background(),
		`SELECT id, description, due_at, recurrence, status
		 FROM tasks
		 WHERE status = 'pending' AND due_at IS NOT NULL
		 ORDER BY due_at ASC
		 LIMIT 1`)

	var t Task
	var recurrenceNs int64
	err := row.Scan(&t.ID, &t.Description, &t.DueAt, &recurrenceNs, &t.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	t.Recurrence = time.Duration(recurrenceNs)
	return &t, nil
}

// executeTask prompts the LLM with the due task, sends the reply to the user,
// marks the task as completed in the database, and handles recurrence by
// inserting a new pending row for the next occurrence.
func (e *Engine) executeTask(task Task) {
	// Verify the task is still pending (it may have been completed externally).
	var status string
	err := e.DB.QueryRow(context.Background(),
		`SELECT status FROM tasks WHERE id = $1`, task.ID).Scan(&status)
	if err != nil || status != "pending" {
		logger.Log.Infof("[scheduler] task #%d no longer pending, skipping", task.ID)
		return
	}

	logger.Log.Infof("[scheduler] executing task #%d: %s", task.ID, task.Description)

	prompt := fmt.Sprintf(
		"[SCHEDULED TASK DUE] The following task is now due: %q. "+
			"Respond directly to the user with a short message fulfilling this task. "+
			"Do NOT call complete_task, update_state, add_task, or save_memory — the system handles task lifecycle automatically. "+
			"Current time: %s",
		task.Description, timefmt.Now(),
	)

	var reply string
	var msgs []llm.Message
	e.withOrchestratorLock(func() {
		opts := e.baseOrchestratorOpts()
		opts.History = e.SM.ReadMessages()
		reply, msgs, err = llm.QueryOrchestrator(context.Background(), e.LLM.Client, e.LLM.Model, prompt, e.ToolExec, DefaultTrimFn, opts)
	})

	for _, m := range msgs {
		e.SM.AppendMessage(m)
	}

	if err != nil {
		logger.Log.Errorf("[scheduler] task #%d LLM error: %v", task.ID, err)
		reply = fmt.Sprintf("(Scheduled task %q fired but LLM error occurred: %v)", task.Description, err)
	}

	if e.SendFunc != nil && reply != "" {
		e.SendFunc(reply)
	}

	// Mark completed (+ insert recurring) in a single transaction.
	txCtx, txCancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer txCancel()

	tx, txErr := e.DB.Begin(txCtx)
	if txErr != nil {
		logger.Log.Errorf("[scheduler] failed to begin task completion tx: %v", txErr)
		return
	}
	defer tx.Rollback(txCtx)

	if _, txErr = tx.Exec(txCtx,
		`UPDATE tasks SET status = 'completed' WHERE id = $1`, task.ID); txErr != nil {
		logger.Log.Errorf("[scheduler] failed to mark task #%d completed: %v", task.ID, txErr)
		return
	}

	// Handle recurrence: insert a new pending row for the next occurrence.
	if task.Recurrence > 0 && task.DueAt != nil {
		nextDue := task.DueAt.Add(task.Recurrence)
		if _, txErr = tx.Exec(txCtx,
			`INSERT INTO tasks (description, due_at, recurrence, status) VALUES ($1, $2, $3, 'pending')`,
			task.Description, nextDue, int64(task.Recurrence)); txErr != nil {
			logger.Log.Errorf("[scheduler] failed to insert recurring task: %v", txErr)
			return
		}
		logger.Log.Infof("[scheduler] recurring task %q rescheduled for %s", task.Description, nextDue.Format(time.RFC3339))
	}

	if txErr = tx.Commit(txCtx); txErr != nil {
		logger.Log.Errorf("[scheduler] task completion commit failed: %v", txErr)
	}
}

// FetchPendingTasksMarkdown queries the database for all pending tasks and
// returns a Markdown-formatted summary suitable for inclusion in prompts.
func FetchPendingTasksMarkdown(ctx context.Context, pool *pgxpool.Pool) string {
	rows, err := pool.Query(ctx,
		`SELECT id, description, due_at, recurrence FROM tasks WHERE status = 'pending' ORDER BY due_at ASC NULLS LAST`)
	if err != nil {
		return "**Task Queue:** (error fetching tasks)\n"
	}
	defer rows.Close()

	var b strings.Builder
	b.WriteString("**Task Queue:**\n")
	count := 0
	for rows.Next() {
		var id int64
		var desc string
		var dueAt *time.Time
		var recurrenceNs int64
		if err := rows.Scan(&id, &desc, &dueAt, &recurrenceNs); err != nil {
			continue
		}
		recurrence := time.Duration(recurrenceNs)
		count++
		switch {
		case dueAt != nil && recurrence > 0:
			fmt.Fprintf(&b, "- [%d] %s (due: %s, every %s)\n", id, desc, dueAt.Format(time.RFC3339), recurrence)
		case dueAt != nil:
			fmt.Fprintf(&b, "- [%d] %s (due: %s)\n", id, desc, dueAt.Format(time.RFC3339))
		case recurrence > 0:
			fmt.Fprintf(&b, "- [%d] %s (every %s)\n", id, desc, recurrence)
		default:
			fmt.Fprintf(&b, "- [%d] %s\n", id, desc)
		}
	}
	if count == 0 {
		b.WriteString("- (empty)\n")
	}
	return b.String()
}
