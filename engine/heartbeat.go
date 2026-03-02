package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sokratos/logger"
	"sokratos/textutil"
	"sokratos/timefmt"
	"sokratos/timeouts"
)

// dueRoutine represents a single routine row that's due for execution.
type dueRoutine struct {
	ID            int
	Name          string
	Instruction   string
	Tool          *string // if set, call this tool directly
	Goal          *string // what to do with tool results
	SilentIfEmpty bool    // skip orchestrator if tool returns empty
}

// heartbeatTask represents a pending task for heartbeat context assembly.
type heartbeatTask struct {
	ID          int64
	Description string
	DueAt       *time.Time
}

// backgroundTask represents a background plan_and_execute task for heartbeat context.
type backgroundTask struct {
	ID        int64
	Directive string
	Status    string
	Priority  int
	Progress  string // "2/5"
	ErrMsg    *string
}

// heartbeatContext holds working memory gathered from Postgres for Phase 2.
type heartbeatContext struct {
	currentTime      string
	currentObjective string // from e.SM.GetState().CurrentTask
	userLastActive   string // RFC3339 timestamp of last user message
	tasks            []heartbeatTask
	backgroundTasks  []backgroundTask
	recentActions    []actionRecord
}

// heartbeatTick handles a single heartbeat using a two-phase approach:
// Phase 1 executes due routines deterministically, then Phase 2 runs
// contextual orchestrator reasoning over working memory.
func (e *Engine) heartbeatTick() {
	defer func() {
		if r := recover(); r != nil {
			logger.Log.Errorf("heartbeat: panic recovered: %v", r)
		}
	}()

	if e.LLM.Client == nil {
		logger.Log.Warn("heartbeat: llm.Client is nil, skipping tick")
		return
	}

	// Hot-reload skills + routines from disk.
	if e.SyncFunc != nil {
		e.SyncFunc()
	}

	// Refresh user preferences from DB (picks up externally added prefs).
	e.SM.RefreshPrefs()

	// === PHASE 1: Deterministic Routine Execution ===
	routinesFired := 0
	if e.DB != nil {
		routinesFired = e.executeDueRoutines()
	}

	// Build temporal context for system prompt injection.
	if e.DB != nil {
		e.TemporalContent = BuildTemporalContext(context.Background(), e.DB)
	}

	// === PHASE 2: Contextual Reasoning ===
	hbCtx := e.gatherHeartbeatContext()
	contextXML := hbCtx.toXML()

	// Staleness detection: if the user hasn't sent a message recently,
	// exclude conversation history to prevent the model from trying to
	// continue or rehash stale conversations.
	lastActivity := e.SM.LastUserActivity()
	staleThreshold := 2 * e.Interval
	if staleThreshold < 10*time.Minute {
		staleThreshold = 10 * time.Minute
	}
	conversationStale := !lastActivity.IsZero() && time.Since(lastActivity) > staleThreshold

	// Build staleness context.
	var stalenessNote string
	if lastActivity.IsZero() {
		stalenessNote = "The user has not sent any messages this session. Do NOT initiate conversation."
	} else if conversationStale {
		stalenessNote = fmt.Sprintf("The user has been inactive for %s. The conversation is STALE — do NOT continue, revisit, or follow up on it.", time.Since(lastActivity).Truncate(time.Minute))
	} else {
		stalenessNote = "The user is actively chatting. Only take proactive action if truly urgent."
	}

	if e.Gatekeeper != nil {
		e.heartbeatPhase2Gatekeeper(contextXML, stalenessNote, conversationStale)
	} else {
		e.heartbeatPhase2Orchestrator(contextXML, stalenessNote, conversationStale)
	}

	if e.DB != nil && e.EmbedEndpoint != "" {
		SlideAndArchiveContext(context.Background(), e.SM, e.MaxMessages, e.archiveDeps())
	}

	// Phase 3: Periodic maintenance (decay + pruning).
	e.runMaintenanceIfDue()

	// Phase 4: Event-driven cognitive processing.
	e.runCognitiveIfTriggered()

	logger.Log.Infof("heartbeat: tick complete, routines_fired=%d", routinesFired)
}

// gatherHeartbeatContext queries Postgres for pending tasks and background tasks.
// Recent salient memories are provided by <temporal_context> in the system prompt.
// Each query uses a 5-second timeout. On any query failure the affected section
// is left empty — the orchestrator still runs with partial context.
func (e *Engine) gatherHeartbeatContext() heartbeatContext {
	hc := heartbeatContext{
		currentTime:      timefmt.Now(),
		currentObjective: e.SM.GetState().CurrentTask,
	}
	if la := e.SM.LastUserActivity(); !la.IsZero() {
		hc.userLastActive = la.Format(time.RFC3339)
	}

	hc.recentActions = e.recentActions

	if e.DB == nil {
		return hc
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
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

	// Query 2: Background tasks (running + recently completed within 1h).
	// Note: recent salient memories are provided by <temporal_context> in the
	// system prompt (7-day window, salience≥6, up to 8 items) — no need to
	// duplicate them here.
	bgRows, err := e.DB.Query(queryCtx,
		`SELECT id, directive, status, COALESCE(priority, 5), steps_total, steps_completed, error_message
		 FROM background_tasks
		 WHERE status = 'running'
		    OR (status IN ('completed', 'failed') AND completed_at >= NOW() - INTERVAL '1 hour')
		 ORDER BY
		    CASE WHEN status = 'running' THEN 0 ELSE 1 END,
		    priority DESC,
		    created_at DESC
		 LIMIT 5`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query background tasks: %v", err)
	} else {
		for bgRows.Next() {
			var bt backgroundTask
			var stepsTotal, stepsCompleted int
			if err := bgRows.Scan(&bt.ID, &bt.Directive, &bt.Status, &bt.Priority, &stepsTotal, &stepsCompleted, &bt.ErrMsg); err != nil {
				logger.Log.Warnf("heartbeat: failed to scan background task row: %v", err)
				continue
			}
			bt.Progress = fmt.Sprintf("%d/%d", stepsCompleted, stepsTotal)
			hc.backgroundTasks = append(hc.backgroundTasks, bt)
		}
		bgRows.Close()
	}

	return hc
}

// toXML formats the heartbeat context as a dense XML block. Empty sections
// use "none" rather than being omitted so the orchestrator sees a consistent
// structure on every tick.
func (hc heartbeatContext) toXML() string {
	var b strings.Builder
	b.WriteString("<heartbeat_context>\n")
	fmt.Fprintf(&b, "  <current_time>%s</current_time>\n", hc.currentTime)

	objective := hc.currentObjective
	if objective == "" {
		objective = "none"
	}
	fmt.Fprintf(&b, "  <current_objective>%s</current_objective>\n", objective)

	lastActive := hc.userLastActive
	if lastActive == "" {
		lastActive = "never"
	}
	fmt.Fprintf(&b, "  <user_last_active>%s</user_last_active>\n", lastActive)

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

	// Background tasks.
	if len(hc.backgroundTasks) == 0 {
		b.WriteString("  <background_tasks>none</background_tasks>\n")
	} else {
		b.WriteString("  <background_tasks>\n")
		for _, bt := range hc.backgroundTasks {
			errAttr := ""
			if bt.ErrMsg != nil && *bt.ErrMsg != "" {
				errAttr = fmt.Sprintf(" error=%q", *bt.ErrMsg)
			}
			dir := textutil.Truncate(bt.Directive, 77)
			fmt.Fprintf(&b, "    <bg_task id=\"%d\" status=\"%s\" priority=\"%d\" progress=\"%s\"%s>%s</bg_task>\n",
				bt.ID, bt.Status, bt.Priority, bt.Progress, errAttr, dir)
		}
		b.WriteString("  </background_tasks>\n")
	}

	// Recent actions taken by this engine.
	if len(hc.recentActions) == 0 {
		b.WriteString("  <recent_actions>none</recent_actions>\n")
	} else {
		b.WriteString("  <recent_actions>\n")
		for _, a := range hc.recentActions {
			fmt.Fprintf(&b, "    <action type=%q time=%q>%s</action>\n",
				a.Type, a.Time.Format(time.RFC3339), a.Summary)
		}
		b.WriteString("  </recent_actions>\n")
	}

	b.WriteString("</heartbeat_context>")
	return b.String()
}
