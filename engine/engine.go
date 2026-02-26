package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
)

// Engine holds all dependencies for the heartbeat loop.
type Engine struct {
	Client                *llm.Client
	Model                 string
	ToolExec              func(context.Context, json.RawMessage) (string, error)
	Mu                    *sync.Mutex
	Interval              time.Duration
	ConsolidationInterval time.Duration // how often to inject the consolidation tick (default 1h)
	SM                    *StateManager
	DB                    *pgxpool.Pool // nil when running without database
	EmbedEndpoint         string        // empty when embeddings unavailable
	EmbedModel            string        // model name for embedding endpoint
	MaxMessages           int           // context window cap for slide (e.g. 20)
	Grammar               string        // GBNF grammar for tool-call constraint
	ProfileContent        string        // identity profile JSON for system prompt injection
	MaxToolResultLen      int           // max chars per tool result (0 = default 2000)
	MaxWebSources         int           // replaces %MAX_WEB_SOURCES% in system prompt (0 = default 2)
	ToolAgent             *llm.ToolAgentConfig // when set, enables the supervisor pattern
	MemoryStalenessDays       int               // prune stale memories older than this many days (0 = disabled)
	SendFunc                  func(text string) // sends a message to the user via Telegram
	InterruptChan             chan struct{}      // signals the task scheduler to recalculate
	EpisodeSynthesisInterval    time.Duration                                                                         // how often to run episode synthesis (default 6h)
	ReflectionMemoryThreshold  int                                                                                    // run reflection after this many new memories (default 50, 0 = disabled)
	ReflectionPrompt           string                                                                                // system prompt for reflection synthesis
	SynthesizeFunc             func(ctx context.Context, systemPrompt, content string) (string, error) // LLM call for synthesis
}

// Run starts a blocking loop that fires at the given interval. Each tick, it
// reads the current agent state, builds a heartbeat prompt with the state
// in Markdown, and sends it to the LLM orchestrator. A separate hourly ticker
// injects silent consolidation events. It serializes LLM access through Mu.
// After each LLM call it attempts to slide and archive old context.
// If a database is available, it starts a PostgreSQL-backed task scheduler
// goroutine alongside the heartbeat loop.
// Intended to be called as a goroutine.
func (e *Engine) Run() {
	// Load identity profile from DB on startup.
	e.RefreshProfile()

	// Start the DB-backed task scheduler.
	if e.DB != nil && e.InterruptChan != nil {
		go e.runTaskScheduler()
	}

	heartbeat := time.NewTicker(e.Interval)
	defer heartbeat.Stop()

	// Consolidation ticker defaults to 1 hour if not set.
	consolidationInterval := e.ConsolidationInterval
	if consolidationInterval <= 0 {
		consolidationInterval = 1 * time.Hour
	}
	consolidation := time.NewTicker(consolidationInterval)
	defer consolidation.Stop()

	// Episode synthesis ticker defaults to 6 hours.
	episodeInterval := e.EpisodeSynthesisInterval
	if episodeInterval <= 0 {
		episodeInterval = 6 * time.Hour
	}
	episodeTicker := time.NewTicker(episodeInterval)
	defer episodeTicker.Stop()

	logger.Log.Infof("[engine] heartbeat started (interval: %s, consolidation: %s, episodes: %s, reflection threshold: %d memories)",
		e.Interval, consolidationInterval, episodeInterval, e.ReflectionMemoryThreshold)

	for {
		select {
		case <-heartbeat.C:
			e.heartbeatTick()
		case <-consolidation.C:
			e.consolidationTick()
		case <-episodeTicker.C:
			e.episodeSynthesisTick()
		}
	}
}

// RefreshProfile loads the identity profile from the database into the engine's
// ProfileContent field. Called on startup and after consolidation runs.
func (e *Engine) RefreshProfile() {
	if e.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutDBQuery)
	defer cancel()
	content, err := memory.GetIdentityProfile(ctx, e.DB)
	if err != nil {
		logger.Log.Warnf("[engine] failed to refresh profile: %v", err)
		return
	}
	e.ProfileContent = content
}

// heartbeatPrefetch embeds the current task and retrieves semantically similar
// memories as background context for the heartbeat. Returns nil if the current
// task is empty, embedding fails, or no memories match.
func (e *Engine) heartbeatPrefetch(ctx context.Context) *llm.Message {
	task := e.SM.GetState().CurrentTask
	if strings.TrimSpace(task) == "" {
		return nil
	}

	embedCtx, cancel := context.WithTimeout(ctx, TimeoutEmbedding)
	defer cancel()

	pf := memory.Prefetch(embedCtx, e.DB, e.EmbedEndpoint, e.EmbedModel, task, task, 3)
	if pf == nil {
		return nil
	}

	// Bump retrieval stats in background.
	go func() {
		bCtx, bCancel := context.WithTimeout(context.Background(), TimeoutDBQuery)
		defer bCancel()
		_, _ = e.DB.Exec(bCtx,
			`UPDATE memories
			 SET retrieval_count = COALESCE(retrieval_count, 0) + 1,
			     last_retrieved_at = NOW(),
			     last_accessed = NOW(),
			     salience = LEAST(COALESCE(salience, 5) + (0.3 * (1.0 - COALESCE(salience, 5) / 10.0)), 10)
			 WHERE id = ANY($1)`,
			pf.IDs,
		)
	}()

	logger.Log.Infof("[engine] heartbeat prefetch injected %d memories", len(pf.IDs))
	return &llm.Message{Role: "system", Content: pf.Content}
}

// dueDirective represents a single directive row that's due for execution.
type dueDirective struct {
	ID          int
	Name        string
	Instruction string
}

// heartbeatTask represents a pending task for heartbeat context assembly.
type heartbeatTask struct {
	ID          int64
	Description string
	DueAt       *time.Time
}

// heartbeatMemory represents a recent salient memory for heartbeat context.
type heartbeatMemory struct {
	Summary   string
	CreatedAt time.Time
}

// heartbeatContext holds working memory gathered from Postgres for Phase 2.
type heartbeatContext struct {
	currentTime string
	tasks       []heartbeatTask
	memories    []heartbeatMemory
}

// executivePrompt is injected as a system message between the standard system
// prompt and the heartbeat context XML for Phase 2 contextual reasoning.
const executivePrompt = `You are running your autonomous background loop. Directives have already been executed for this tick. Review the <heartbeat_context> and determine if any proactive action is required.

Priority order:
1. Pending tasks that are overdue or due within 24 hours — take action if you can make progress without user input.
2. If recent memories suggest something time-sensitive needs attention (upcoming travel, an open promise to follow up, an approaching deadline), address it.
3. Only message the user if there is something they need to know or decide RIGHT NOW. Do not message the user to report that background tasks completed silently.

CRITICAL: You MUST respond with exactly ONE of these two formats — anything else will be sent directly to the user as a Telegram message:
- If no action is needed: <NO_ACTION_REQUIRED>
- If action is needed: <TOOL_INTENT>describe the exact action to take</TOOL_INTENT>

Do NOT output status words like "idle", acknowledgements, or commentary. Your entire response must be one of the two tags above.`

// heartbeatTick handles a single heartbeat using a two-phase approach:
// Phase 1 executes due directives deterministically, then Phase 2 runs
// contextual orchestrator reasoning over working memory.
func (e *Engine) heartbeatTick() {
	defer func() {
		if r := recover(); r != nil {
			logger.Log.Errorf("heartbeat: panic recovered: %v", r)
		}
	}()

	if e.Client == nil {
		logger.Log.Warn("heartbeat: llm.Client is nil, skipping tick")
		return
	}

	// Refresh user preferences from DB (picks up externally added prefs).
	e.SM.RefreshPrefs()

	// === PHASE 1: Deterministic Directive Execution ===
	directivesFired := 0
	if e.DB != nil {
		directivesFired = e.executeDueDirectives()
	}

	// === PHASE 2: Contextual Orchestrator Reasoning ===
	hbCtx := e.gatherHeartbeatContext()
	contextXML := hbCtx.toXML()

	// Build history: conversation history + executive prompt injection.
	// The executive prompt is a system message placed after conversation
	// history and before the user message (heartbeat context XML).
	trimFn := func(msgs []llm.Message) []llm.Message {
		return TrimMessages(msgs, 12)
	}

	var reply string
	var msgs []llm.Message
	var err error
	func() {
		e.Mu.Lock()
		defer e.Mu.Unlock()

		convHistory := e.SM.ReadMessages()
		history := make([]llm.Message, 0, len(convHistory)+1)
		history = append(history, convHistory...)
		history = append(history, llm.Message{
			Role:    "system",
			Content: executivePrompt,
		})

		reply, msgs, err = llm.QueryOrchestrator(
			context.Background(), e.Client, e.Model, contextXML,
			e.ToolExec, trimFn, &llm.QueryOrchestratorOpts{
				History:          history,
				Grammar:          e.Grammar,
				ProfileContent:   e.ProfileContent,
				MaxToolResultLen: e.MaxToolResultLen,
				MaxWebSources:    e.MaxWebSources,
				ToolAgent:        e.ToolAgent,
			},
		)
	}()

	// Persist Phase 2 messages for conversation continuity.
	for _, m := range msgs {
		e.SM.AppendMessage(m)
	}

	// Route the orchestrator's response.
	switch {
	case err != nil:
		if strings.Contains(err.Error(), "too many tool call rounds") {
			logger.Log.Warn("heartbeat: max rounds reached")
			if e.SendFunc != nil {
				e.SendFunc("I started a background task but couldn't complete it. You may want to check in.")
			}
		} else {
			logger.Log.Errorf("heartbeat: orchestrator error: %v", err)
		}
	case strings.Contains(reply, "<NO_ACTION_REQUIRED>"):
		logger.Log.Info("heartbeat: no action required")
	case strings.TrimSpace(reply) != "":
		// Orchestrator produced a proactive response — deliver to user.
		if e.SendFunc != nil {
			e.SendFunc(reply)
		}
		logger.Log.Infof("heartbeat: proactive response delivered")
	default:
		logger.Log.Debug("heartbeat: orchestrator produced unexpected output")
	}

	if e.DB != nil && e.EmbedEndpoint != "" {
		SlideAndArchiveContext(context.Background(), e.SM, e.MaxMessages, e.DB, e.EmbedEndpoint, e.EmbedModel)
	}

	logger.Log.Infof("heartbeat: tick complete, directives_fired=%d", directivesFired)
}

// executeDueDirectives queries all directives whose interval has elapsed and
// executes each one independently through the orchestrator. Returns the count
// of directives fired.
func (e *Engine) executeDueDirectives() int {
	queryCtx, cancel := context.WithTimeout(context.Background(), TimeoutDBQuery)
	defer cancel()

	rows, err := e.DB.Query(queryCtx,
		`SELECT id, name, instruction
		 FROM directives
		 WHERE last_executed + interval_duration <= NOW()
		 ORDER BY (last_executed + interval_duration) ASC`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query due directives: %v", err)
		return 0
	}

	var directives []dueDirective
	for rows.Next() {
		var d dueDirective
		if err := rows.Scan(&d.ID, &d.Name, &d.Instruction); err != nil {
			logger.Log.Warnf("heartbeat: failed to scan directive row: %v", err)
			continue
		}
		directives = append(directives, d)
	}
	rows.Close()

	if rows.Err() != nil {
		logger.Log.Warnf("heartbeat: directive iteration error: %v", rows.Err())
	}

	for _, d := range directives {
		e.executeSingleDirective(d)
	}

	return len(directives)
}

// executeSingleDirective advances the directive's timer and runs it through
// the full orchestrator/supervisor loop. Recovers from panics so a single
// failed directive never crashes the heartbeat goroutine.
func (e *Engine) executeSingleDirective(d dueDirective) {
	defer func() {
		if r := recover(); r != nil {
			logger.Log.Errorf("heartbeat: directive panic, name=%s, err=%v", d.Name, r)
		}
	}()

	// Advance timer BEFORE execution to prevent double-fire on crash.
	advCtx, advCancel := context.WithTimeout(context.Background(), TimeoutDBQuery)
	defer advCancel()
	if _, err := e.DB.Exec(advCtx,
		`UPDATE directives SET last_executed = NOW() WHERE id = $1`, d.ID); err != nil {
		logger.Log.Warnf("heartbeat: failed to advance directive timer, name=%s, err=%v", d.Name, err)
		return
	}

	prompt := fmt.Sprintf(
		"DIRECTIVE: %s\nExecute this directive now. Use your tools to complete it.\n"+
			"Do not message the user unless the directive explicitly requires it.",
		d.Instruction,
	)

	trimFn := func(msgs []llm.Message) []llm.Message {
		return TrimMessages(msgs, 12)
	}

	var err error
	func() {
		e.Mu.Lock()
		defer e.Mu.Unlock()
		_, _, err = llm.QueryOrchestrator(
			context.Background(), e.Client, e.Model, prompt,
			e.ToolExec, trimFn, &llm.QueryOrchestratorOpts{
				Grammar:          e.Grammar,
				ProfileContent:   e.ProfileContent,
				MaxToolResultLen: e.MaxToolResultLen,
				MaxWebSources:    e.MaxWebSources,
				ToolAgent:        e.ToolAgent,
			},
		)
	}()

	if err != nil {
		logger.Log.Warnf("heartbeat: directive failed, name=%s, err=%v", d.Name, err)
		return
	}

	logger.Log.Infof("heartbeat: directive executed, name=%s", d.Name)
}

// gatherHeartbeatContext queries Postgres for pending tasks and recent salient
// memories. Each query uses a 5-second timeout. On any query failure the
// affected section is left empty — the orchestrator still runs with partial context.
func (e *Engine) gatherHeartbeatContext() heartbeatContext {
	hc := heartbeatContext{
		currentTime: time.Now().Format(time.RFC3339),
	}

	if e.DB == nil {
		return hc
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), TimeoutDBQuery)
	defer cancel()

	// Query 1: Pending tasks.
	taskRows, err := e.DB.Query(queryCtx,
		`SELECT id, description, due_at
		 FROM tasks
		 WHERE status = 'pending'
		 ORDER BY due_at ASC NULLS LAST
		 LIMIT 5`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query pending tasks: %v", err)
	} else {
		for taskRows.Next() {
			var t heartbeatTask
			if err := taskRows.Scan(&t.ID, &t.Description, &t.DueAt); err != nil {
				logger.Log.Warnf("heartbeat: failed to scan task row: %v", err)
				continue
			}
			hc.tasks = append(hc.tasks, t)
		}
		taskRows.Close()
	}

	// Query 2: Recent salient memories (exclude backfill — those are historical
	// content ingested now, not things that actually happened recently).
	memRows, err := e.DB.Query(queryCtx,
		`SELECT summary, created_at
		 FROM memories
		 WHERE created_at >= NOW() - INTERVAL '48 hours'
		   AND salience >= 7
		   AND COALESCE(source, '') != 'backfill'
		 ORDER BY created_at DESC
		 LIMIT 3`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query recent memories: %v", err)
	} else {
		for memRows.Next() {
			var m heartbeatMemory
			if err := memRows.Scan(&m.Summary, &m.CreatedAt); err != nil {
				logger.Log.Warnf("heartbeat: failed to scan memory row: %v", err)
				continue
			}
			hc.memories = append(hc.memories, m)
		}
		memRows.Close()
	}

	// Query 3: Upcoming calendar events.
	// TODO: implement when local calendar cache table exists.
	// Expected query:
	//   SELECT title, start_time, location
	//   FROM calendar_cache
	//   WHERE start_time BETWEEN NOW() AND NOW() + INTERVAL '48 hours'
	//   ORDER BY start_time ASC LIMIT 5

	return hc
}

// toXML formats the heartbeat context as a dense XML block. Empty sections
// use "none" rather than being omitted so the orchestrator sees a consistent
// structure on every tick.
func (hc heartbeatContext) toXML() string {
	var b strings.Builder
	b.WriteString("<heartbeat_context>\n")
	fmt.Fprintf(&b, "  <current_time>%s</current_time>\n", hc.currentTime)

	// Recent salient memories.
	if len(hc.memories) == 0 {
		b.WriteString("  <recent_salient_memories>none</recent_salient_memories>\n")
	} else {
		b.WriteString("  <recent_salient_memories>\n")
		for _, m := range hc.memories {
			fmt.Fprintf(&b, "    <memory recorded=\"%s\">%s</memory>\n",
				m.CreatedAt.Format(time.RFC3339), m.Summary)
		}
		b.WriteString("  </recent_salient_memories>\n")
	}

	// Pending tasks.
	if len(hc.tasks) == 0 {
		b.WriteString("  <pending_tasks>none</pending_tasks>\n")
	} else {
		b.WriteString("  <pending_tasks>\n")
		for _, t := range hc.tasks {
			due := "none"
			if t.DueAt != nil {
				due = t.DueAt.Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "    <task id=\"%d\" due=\"%s\">%s</task>\n",
				t.ID, due, t.Description)
		}
		b.WriteString("  </pending_tasks>\n")
	}

	// Upcoming calendar (TODO).
	b.WriteString("  <upcoming_calendar>none</upcoming_calendar>\n")

	b.WriteString("</heartbeat_context>")
	return b.String()
}

// consolidationTick injects a silent system event into the sliding window and
// runs the LLM orchestrator so it can assess whether memory consolidation is
// needed. The event message is never sent to Telegram — only the LLM sees it.
func (e *Engine) consolidationTick() {
	currentTime := time.Now().Format("Monday, January 2, 2006 at 3:04 PM")
	prompt := fmt.Sprintf(
		"<system_event>1 hour has passed. Current Time: %s. Assess if memory consolidation is required.</system_event>",
		currentTime,
	)

	logger.Log.Info("[engine] consolidation tick fired")

	// Materialize salience decay before the LLM assesses consolidation.
	if e.DB != nil {
		if n, err := memory.MaterializeDecay(context.Background(), e.DB); err != nil {
			logger.Log.Warnf("[engine] salience decay failed: %v", err)
		} else if n > 0 {
			logger.Log.Infof("[engine] decayed salience for %d memories", n)
		}

		// Prune stale memories that have decayed beyond recovery.
		if e.MemoryStalenessDays > 0 {
			if n, err := memory.PruneStaleMemories(context.Background(), e.DB, e.MemoryStalenessDays); err != nil {
				logger.Log.Warnf("[engine] memory pruning failed: %v", err)
			} else if n > 0 {
				logger.Log.Infof("[engine] pruned %d stale memories", n)
			}
		}
	}

	// Check if enough new memories have accumulated to trigger reflection.
	if e.SynthesizeFunc != nil && e.ReflectionPrompt != "" && e.ReflectionMemoryThreshold > 0 && e.DB != nil {
		count, err := memory.CountMemoriesSinceLastReflection(context.Background(), e.DB)
		if err != nil {
			logger.Log.Warnf("[engine] reflection count check failed: %v", err)
		} else if count >= e.ReflectionMemoryThreshold {
			logger.Log.Infof("[engine] reflection threshold reached (%d >= %d), triggering reflection", count, e.ReflectionMemoryThreshold)
			e.triggerReflection()
		}
	}

	e.Mu.Lock()
	trimFn := func(msgs []llm.Message) []llm.Message {
		return TrimMessages(msgs, 12)
	}
	history := e.SM.ReadMessages()
	reply, msgs, err := llm.QueryOrchestrator(context.Background(), e.Client, e.Model, prompt, e.ToolExec, trimFn, &llm.QueryOrchestratorOpts{History: history, Grammar: e.Grammar, ProfileContent: e.ProfileContent, MaxToolResultLen: e.MaxToolResultLen, MaxWebSources: e.MaxWebSources, ToolAgent: e.ToolAgent})
	e.Mu.Unlock()

	for _, m := range msgs {
		e.SM.AppendMessage(m)
	}

	if err != nil {
		logger.Log.Errorf("[engine] consolidation tick error: %v", err)
	} else {
		logger.Log.Infof("[engine] consolidation: %s", reply)
	}

	// Refresh profile after consolidation in case it was updated.
	e.RefreshProfile()

	if e.DB != nil && e.EmbedEndpoint != "" {
		SlideAndArchiveContext(context.Background(), e.SM, e.MaxMessages, e.DB, e.EmbedEndpoint, e.EmbedModel)
	}
}

// episodeSynthesisTick clusters recent semantically-similar memories and
// synthesizes them into episodic memories. Skipped if prerequisites are missing.
func (e *Engine) episodeSynthesisTick() {
	if e.DB == nil || e.EmbedEndpoint == "" || e.SynthesizeFunc == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), TimeoutSynthesis)
	defer cancel()

	n, err := memory.SynthesizeEpisodes(ctx, e.DB, e.EmbedEndpoint, e.EmbedModel, e.SynthesizeFunc)
	if err != nil {
		logger.Log.Warnf("[engine] episode synthesis failed: %v", err)
	} else if n > 0 {
		logger.Log.Infof("[engine] synthesized %d episodes", n)
	}
}

// triggerReflection performs a meta-cognitive reflection over memories since
// the last reflection, identifying patterns and predictions. Called when the
// memory count threshold is reached during consolidationTick.
func (e *Engine) triggerReflection() {
	if e.DB == nil || e.EmbedEndpoint == "" || e.SynthesizeFunc == nil || e.ReflectionPrompt == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), TimeoutSynthesis)
	defer cancel()

	// Determine the window: since the last reflection, or 7 days if none exists.
	since := time.Now().AddDate(0, 0, -7)
	var lastReflection *time.Time
	err := e.DB.QueryRow(ctx,
		`SELECT MAX(created_at) FROM memories WHERE memory_type = 'reflection'`,
	).Scan(&lastReflection)
	if err == nil && lastReflection != nil && !lastReflection.IsZero() {
		since = *lastReflection
	}

	id, err := memory.ReflectOnMemories(ctx, e.DB, e.EmbedEndpoint, e.EmbedModel, e.ReflectionPrompt, e.SynthesizeFunc, since)
	if err != nil {
		logger.Log.Warnf("[engine] reflection failed: %v", err)
	} else if id > 0 {
		logger.Log.Infof("[engine] reflection saved as memory id=%d", id)
	}
}

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
		task.Description, time.Now().Format(time.RFC3339),
	)

	e.Mu.Lock()
	trimFn := func(msgs []llm.Message) []llm.Message {
		return TrimMessages(msgs, 12)
	}
	history := e.SM.ReadMessages()
	reply, msgs, err := llm.QueryOrchestrator(context.Background(), e.Client, e.Model, prompt, e.ToolExec, trimFn, &llm.QueryOrchestratorOpts{History: history, Grammar: e.Grammar, ProfileContent: e.ProfileContent, MaxToolResultLen: e.MaxToolResultLen, MaxWebSources: e.MaxWebSources, ToolAgent: e.ToolAgent})
	e.Mu.Unlock()

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

	// Mark task as completed.
	if _, dbErr := e.DB.Exec(context.Background(),
		`UPDATE tasks SET status = 'completed' WHERE id = $1`, task.ID); dbErr != nil {
		logger.Log.Errorf("[scheduler] failed to mark task #%d completed: %v", task.ID, dbErr)
	}

	// Handle recurrence: insert a new pending row for the next occurrence.
	if task.Recurrence > 0 && task.DueAt != nil {
		nextDue := task.DueAt.Add(task.Recurrence)
		if _, dbErr := e.DB.Exec(context.Background(),
			`INSERT INTO tasks (description, due_at, recurrence, status) VALUES ($1, $2, $3, 'pending')`,
			task.Description, nextDue, int64(task.Recurrence)); dbErr != nil {
			logger.Log.Errorf("[scheduler] failed to insert recurring task: %v", dbErr)
		} else {
			logger.Log.Infof("[scheduler] recurring task %q rescheduled for %s", task.Description, nextDue.Format(time.RFC3339))
		}
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
