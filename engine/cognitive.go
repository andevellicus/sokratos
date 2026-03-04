package engine

import (
	"context"
	"strconv"
	"strings"
	"time"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/timeouts"
)

// runMaintenanceIfDue runs lightweight maintenance (salience decay, stale memory
// pruning, table housekeeping) at most every MaintenanceInterval.
func (e *Engine) runMaintenanceIfDue() {
	if e.DB == nil || time.Since(e.lastMaintenanceRun) < e.MaintenanceInterval {
		return
	}
	e.lastMaintenanceRun = time.Now()
	ctx := context.Background()

	// --- Memory maintenance ---

	if n, err := memory.MaterializeDecay(ctx, e.DB); err != nil {
		logger.Log.Warnf("[engine] salience decay failed: %v", err)
	} else if n > 0 {
		logger.Log.Infof("[engine] decayed salience for %d memories", n)
	}

	if e.MemoryStalenessDays > 0 {
		if n, err := memory.PruneStaleMemories(ctx, e.DB, e.MemoryStalenessDays); err != nil {
			logger.Log.Warnf("[engine] memory pruning failed: %v", err)
		} else if n > 0 {
			logger.Log.Infof("[engine] pruned %d stale memories", n)
		}
	}

	// --- Table housekeeping ---
	// Prune rows from tables that grow without bounds. Each query is cheap
	// (index-backed timestamp filter) and runs at most every MaintenanceInterval.
	// TTLs are configurable via environment variables (0 = disabled).

	type pruneSpec struct {
		label string
		query string
		days  int
	}
	for _, pq := range []pruneSpec{
		{"work_items", `DELETE FROM work_items WHERE status IN ('completed','failed','cancelled') AND completed_at < now() - ($1 || ' days')::interval`, e.WorkItemsTTLDays},
		{"processed_emails", `DELETE FROM processed_emails WHERE seen_at < now() - ($1 || ' days')::interval`, e.ProcessedEmailsTTLDays},
		{"processed_events", `DELETE FROM processed_events WHERE seen_at < now() - ($1 || ' days')::interval`, e.ProcessedEventsTTLDays},
		{"failed_operations", `DELETE FROM failed_operations WHERE created_at < now() - ($1 || ' days')::interval`, e.FailedOpsTTLDays},
		{"skill_kv", `DELETE FROM skill_kv WHERE updated_at < now() - ($1 || ' days')::interval`, e.SkillKVTTLDays},
		{"shell_history", `DELETE FROM shell_history WHERE created_at < now() - ($1 || ' days')::interval`, e.ShellHistoryTTLDays},
	} {
		if pq.days <= 0 {
			continue
		}
		res, err := e.DB.Exec(ctx, pq.query, strconv.Itoa(pq.days))
		if err != nil {
			logger.Log.Warnf("[engine] %s pruning failed: %v", pq.label, err)
		} else if n := res.RowsAffected(); n > 0 {
			logger.Log.Infof("[engine] pruned %d rows from %s", n, pq.label)
		}
	}

	logger.Log.Info("[engine] maintenance complete")
}

// runCognitiveIfTriggered evaluates volume + lull + ceiling triggers and runs
// heavy cognitive processing: reflection, episode synthesis, profile consolidation.
func (e *Engine) runCognitiveIfTriggered() {
	if e.DB == nil || e.EmbedEndpoint == "" {
		return
	}

	count, err := memory.CountMemoriesSince(context.Background(), e.DB, e.lastCognitiveRun)
	if err != nil {
		logger.Log.Warnf("[engine] cognitive: memory count failed: %v", err)
		return
	}

	bufferFull := count >= e.Cognitive.BufferThreshold
	lastActivity := e.SM.LastUserActivity()
	lull := lastActivity.IsZero() || time.Since(lastActivity) >= e.Cognitive.LullDuration
	ceilingHit := time.Since(e.lastCognitiveRun) >= e.Cognitive.Ceiling

	if !((bufferFull && lull) || ceilingHit) {
		return
	}

	logger.Log.Infof("[engine] cognitive processing triggered (count=%d, buffer_full=%v, lull=%v, ceiling=%v)",
		count, bufferFull, lull, ceilingHit)

	// 1. Reflection (if unreflected count meets the reflection-specific threshold).
	if e.Cognitive.SynthesizeFunc != nil && e.Cognitive.ReflectionPrompt != "" && e.Cognitive.ReflectionMemoryThreshold > 0 {
		reflCount, reflErr := memory.CountMemoriesSinceLastReflection(context.Background(), e.DB)
		if reflErr == nil && reflCount >= e.Cognitive.ReflectionMemoryThreshold {
			logger.Log.Infof("[engine] reflection threshold reached (%d >= %d)", reflCount, e.Cognitive.ReflectionMemoryThreshold)
			e.triggerReflection()
		}
	}

	// 2. Episode synthesis.
	if e.Cognitive.SynthesizeFunc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeouts.Synthesis)
		n, synthErr := memory.SynthesizeEpisodes(ctx, e.DB, e.EmbedEndpoint, e.EmbedModel, e.Cognitive.SynthesizeFunc, e.GrammarFunc)
		cancel()
		if synthErr != nil {
			logger.Log.Warnf("[engine] episode synthesis failed: %v", synthErr)
		} else if n > 0 {
			logger.Log.Infof("[engine] synthesized %d episodes", n)
		}
	}

	// 3. Profile consolidation.
	e.runProfileConsolidation()

	// 4. Proactive curiosity.
	e.runCuriosityIfReady()

	// 4.5. Goal inference.
	e.runGoalInferenceIfReady()

	// 5. Refresh cached profile and personality.
	e.RefreshProfile()
	e.RefreshPersonality()

	e.lastCognitiveRun = time.Now()
}

// runProfileConsolidation calls the ConsolidateFunc closure if configured.
// The volume + lull trigger has already decided consolidation should happen.
func (e *Engine) runProfileConsolidation() {
	if e.Cognitive.ConsolidateFunc == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Synthesis)
	defer cancel()

	n, err := e.Cognitive.ConsolidateFunc(ctx)
	if err != nil {
		logger.Log.Warnf("[engine] profile consolidation failed: %v", err)
		return
	}
	if n > 0 {
		logger.Log.Infof("[engine] consolidated %d memories into profile", n)
	}
}

// triggerReflection performs a meta-cognitive reflection over memories since
// the last reflection, identifying patterns and predictions. Called when the
// memory count threshold is reached during cognitive processing.
func (e *Engine) triggerReflection() {
	if e.DB == nil || e.EmbedEndpoint == "" || e.Cognitive.SynthesizeFunc == nil || e.Cognitive.ReflectionPrompt == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Synthesis)
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

	id, err := memory.ReflectOnMemories(ctx, e.DB, e.EmbedEndpoint, e.EmbedModel, e.Cognitive.ReflectionPrompt, e.Cognitive.SynthesizeFunc, e.GrammarFunc, since)
	if err != nil {
		logger.Log.Warnf("[engine] reflection failed: %v", err)
	} else if id > 0 {
		logger.Log.Infof("[engine] reflection saved as memory id=%d", id)
		// Inject reflection insight into conversation context for the orchestrator.
		if e.ReflectionNotifyFunc != nil {
			rCtx, rCancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
			var summary string
			qErr := e.DB.QueryRow(rCtx, `SELECT summary FROM memories WHERE id = $1`, id).Scan(&summary)
			rCancel()
			if qErr == nil && strings.TrimSpace(summary) != "" {
				e.ReflectionNotifyFunc(summary)
			}
		}
	}
}
