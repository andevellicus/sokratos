package engine

import (
	"context"
	"time"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/timeouts"
)

// runMaintenanceIfDue runs lightweight maintenance (salience decay, stale memory
// pruning) at most every MaintenanceInterval.
func (e *Engine) runMaintenanceIfDue() {
	if e.DB == nil || time.Since(e.lastMaintenanceRun) < e.MaintenanceInterval {
		return
	}
	e.lastMaintenanceRun = time.Now()

	if n, err := memory.MaterializeDecay(context.Background(), e.DB); err != nil {
		logger.Log.Warnf("[engine] salience decay failed: %v", err)
	} else if n > 0 {
		logger.Log.Infof("[engine] decayed salience for %d memories", n)
	}

	if e.MemoryStalenessDays > 0 {
		if n, err := memory.PruneStaleMemories(context.Background(), e.DB, e.MemoryStalenessDays); err != nil {
			logger.Log.Warnf("[engine] memory pruning failed: %v", err)
		} else if n > 0 {
			logger.Log.Infof("[engine] pruned %d stale memories", n)
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
	}
}
