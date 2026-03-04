package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/textutil"
	"sokratos/timefmt"
	"sokratos/timeouts"
)

// activeGoal represents an inferred user goal from memory.
type activeGoal struct {
	ID      int64
	Summary string
}

// workItem represents a row from the unified work_items table for heartbeat
// context assembly.
type workItem struct {
	ID        int64
	Type      string // "scheduled", "background", "routine"
	Directive string
	Status    string
	Priority  int
	Progress  string // "2/5" for background tasks
	DueAt     *time.Time
	StartedAt *time.Time
	ErrMsg    *string
}

// heartbeatContext holds working memory gathered from Postgres for Phase 2.
type heartbeatContext struct {
	currentTime      string
	currentObjective string // from e.SM.GetState().CurrentTask
	userLastActive   string // RFC3339 timestamp of last user message
	workItems        []workItem
	recentActions    []actionRecord
	goals            []activeGoal
}

// heartbeatTick handles a single heartbeat. Routines run on their own
// independent scheduler (see runRoutineScheduler). The heartbeat focuses on
// contextual orchestrator reasoning, maintenance, and cognitive processing.
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

	// Kill hung work items before proceeding.
	if e.WorkMonitor != nil {
		if killed := e.WorkMonitor.KillHungWork(); killed > 0 {
			logger.Log.Warnf("heartbeat: watchdog killed %d hung work item(s)", killed)
		}
	}

	// Hot-reload skills from disk.
	if e.SyncFunc != nil {
		e.SyncFunc()
	}

	// Refresh user preferences from DB (picks up externally added prefs).
	e.SM.RefreshPrefs()

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

	// Goal pursuit: actively work on highest-salience goals.
	e.runGoalPursuitIfReady()

	// Phase 3: Periodic maintenance (decay + pruning).
	e.runMaintenanceIfDue()

	// Phase 4: Event-driven cognitive processing.
	e.runCognitiveIfTriggered()

	logger.Log.Info("heartbeat: tick complete")
}

// gatherHeartbeatContext queries Postgres for work items (scheduled, background,
// routine) and active goals. Recent salient memories are provided by
// <temporal_context> in the system prompt. Each query uses a 5-second timeout.
// On any query failure the affected section is left empty — the orchestrator
// still runs with partial context.
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

	// Unified work items query: running + pending scheduled + recently completed.
	rows, err := e.DB.Query(queryCtx,
		`SELECT id, type, directive, status, COALESCE(priority, 5),
		        steps_total, steps_completed, error_message, due_at, started_at
		 FROM work_items
		 WHERE status = 'running'
		    OR (type = 'scheduled' AND status = 'pending')
		    OR (status IN ('completed', 'failed') AND completed_at >= NOW() - INTERVAL '1 hour')
		 ORDER BY
		    CASE WHEN status = 'running' THEN 0
		         WHEN status = 'pending' THEN 1
		         ELSE 2 END,
		    priority DESC, created_at DESC
		 LIMIT 10`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query work items: %v", err)
	} else {
		for rows.Next() {
			var wi workItem
			var stepsTotal, stepsCompleted int
			if err := rows.Scan(&wi.ID, &wi.Type, &wi.Directive, &wi.Status, &wi.Priority,
				&stepsTotal, &stepsCompleted, &wi.ErrMsg, &wi.DueAt, &wi.StartedAt); err != nil {
				logger.Log.Warnf("heartbeat: failed to scan work item row: %v", err)
				continue
			}
			if stepsTotal > 0 {
				wi.Progress = fmt.Sprintf("%d/%d", stepsCompleted, stepsTotal)
			}
			hc.workItems = append(hc.workItems, wi)
		}
		rows.Close()
	}

	// Active inferred goals (recent, high-salience, non-superseded).
	goalRows, err := e.DB.Query(queryCtx,
		`SELECT id, summary FROM memories
		 WHERE memory_type = 'goal'
		   AND superseded_by IS NULL
		   AND salience >= 6
		   AND created_at >= NOW() - INTERVAL '14 days'
		 ORDER BY salience DESC, created_at DESC
		 LIMIT 3`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query goals: %v", err)
	} else {
		for goalRows.Next() {
			var g activeGoal
			if err := goalRows.Scan(&g.ID, &g.Summary); err != nil {
				logger.Log.Warnf("heartbeat: failed to scan goal row: %v", err)
				continue
			}
			// Skip goals that have recent background task attempts.
			if isGoalAlreadyAttempted(queryCtx, e.DB, g.Summary) {
				continue
			}
			hc.goals = append(hc.goals, g)
		}
		goalRows.Close()
	}

	return hc
}

// cleanGoalSummary strips the "[Inferred Goal] " prefix and "\nEvidence:" suffix
// from a goal memory summary, returning just the goal text.
func cleanGoalSummary(summary string) string {
	clean := summary
	if idx := strings.Index(clean, "[Inferred Goal] "); idx >= 0 {
		clean = clean[idx+len("[Inferred Goal] "):]
	}
	if idx := strings.Index(clean, "\nEvidence:"); idx >= 0 {
		clean = clean[:idx]
	}
	return strings.TrimSpace(clean)
}

// isGoalAlreadyAttempted checks if a work item was recently (24h) started
// for a goal. It strips the "[Inferred Goal]" prefix and evidence, then checks
// for ILIKE match against recent work item directives.
func isGoalAlreadyAttempted(ctx context.Context, db *pgxpool.Pool, goalSummary string) bool {
	clean := cleanGoalSummary(goalSummary)
	if clean == "" {
		return false
	}

	var count int
	err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_items
		 WHERE created_at >= NOW() - INTERVAL '24 hours'
		   AND directive ILIKE '%' || $1 || '%'`, clean).Scan(&count)
	if err != nil {
		return false // fail open
	}
	return count > 0
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

	// Unified work items.
	if len(hc.workItems) == 0 {
		b.WriteString("  <work_items>none</work_items>\n")
	} else {
		b.WriteString("  <work_items>\n")
		for _, wi := range hc.workItems {
			attrs := fmt.Sprintf("id=\"%d\" type=\"%s\" status=\"%s\"", wi.ID, wi.Type, wi.Status)
			if wi.Priority > 0 {
				attrs += fmt.Sprintf(" priority=\"%d\"", wi.Priority)
			}
			if wi.Progress != "" {
				attrs += fmt.Sprintf(" progress=\"%s\"", wi.Progress)
			}
			if wi.DueAt != nil {
				attrs += fmt.Sprintf(" due=\"%s\"", wi.DueAt.Format(time.RFC3339))
			}
			if wi.StartedAt != nil && wi.Status == "running" {
				attrs += fmt.Sprintf(" since=\"%s\"", wi.StartedAt.Format(time.RFC3339))
			}
			if wi.ErrMsg != nil && *wi.ErrMsg != "" {
				attrs += fmt.Sprintf(" error=%q", *wi.ErrMsg)
			}
			dir := textutil.Truncate(wi.Directive, 77)
			fmt.Fprintf(&b, "    <item %s>%s</item>\n", attrs, dir)
		}
		b.WriteString("  </work_items>\n")
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

	// Active inferred goals.
	if len(hc.goals) == 0 {
		b.WriteString("  <active_goals>none</active_goals>\n")
	} else {
		b.WriteString("  <active_goals>\n")
		for _, g := range hc.goals {
			fmt.Fprintf(&b, "    <goal id=\"%d\">%s</goal>\n", g.ID, cleanGoalSummary(g.Summary))
		}
		b.WriteString("  </active_goals>\n")
	}

	b.WriteString("</heartbeat_context>")
	return b.String()
}
