package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	objpkg "sokratos/objectives"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/textutil"
	"sokratos/timefmt"
	"sokratos/timeouts"
)

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
	objectives       []objpkg.Objective
	failures         []memory.FailureSummary
}

// heartbeatTick handles a single heartbeat. Routines run on their own
// independent scheduler (see runRoutineScheduler). The heartbeat focuses on
// contextual orchestrator reasoning, maintenance, and cognitive processing.
func (e *Engine) heartbeatTick() {
	tickStart := time.Now()
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
	if e.Reloader != nil {
		e.Reloader.SyncSkills()
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

	// Objective pursuit: actively work on highest-priority objectives.
	e.runObjectivePursuitIfReady()

	// Drain event-driven curiosity signals.
	e.drainCuriositySignals()

	// Phase 3: Periodic maintenance (decay + pruning).
	e.runMaintenanceIfDue()

	// Phase 4: Event-driven cognitive processing.
	e.runCognitiveIfTriggered()

	gatekeeper := "none"
	if e.Gatekeeper != nil {
		gatekeeper = "present"
	}
	staleStr := "false"
	if conversationStale {
		staleStr = "true"
	}
	e.Metrics.Since("heartbeat.tick", tickStart, map[string]string{
		"gatekeeper": gatekeeper, "stale": staleStr,
	})

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

	// Active objectives from the objectives table.
	activeObjectives, err := objpkg.ListActive(queryCtx, e.DB)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query objectives: %v", err)
	} else {
		hc.objectives = activeObjectives
	}

	// Recent failures from the failed_operations table.
	hc.failures = memory.QueryRecentFailures(queryCtx, e.DB, 6*time.Hour, 5)

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

	// Active objectives with lifecycle info.
	if len(hc.objectives) == 0 {
		b.WriteString("  <active_objectives>none</active_objectives>\n")
	} else {
		b.WriteString("  <active_objectives>\n")
		for _, g := range hc.objectives {
			attrs := fmt.Sprintf("id=\"%d\" status=\"%s\" priority=\"%s\" attempts=\"%d\"",
				g.ID, g.Status, g.Priority, g.Attempts)
			if g.LastPursued != nil {
				attrs += fmt.Sprintf(" last_pursued=\"%s\"", g.LastPursued.Format(time.RFC3339))
			}
			fmt.Fprintf(&b, "    <objective %s>%s</objective>\n", attrs, g.Summary)
		}
		b.WriteString("  </active_objectives>\n")
	}

	// Recent background failures.
	if len(hc.failures) > 0 {
		b.WriteString("  <recent_failures>\n")
		for _, f := range hc.failures {
			fmt.Fprintf(&b, "    <failure type=%q count=\"%d\" last=%q>%s</failure>\n",
				f.OpType, f.Count, f.LastSeen.Format(time.RFC3339), textutil.Truncate(f.LastError, 120))
		}
		b.WriteString("  </recent_failures>\n")
	}

	b.WriteString("</heartbeat_context>")
	return b.String()
}
