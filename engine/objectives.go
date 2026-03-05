package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/objectives"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timeouts"
	"sokratos/tokens"
)

const (
	defaultObjectiveInferenceCooldown = 8 * time.Hour
	objectiveInferenceMaxInactive     = 24 * time.Hour
	objectiveInferenceMinMemories     = 10
)

// ObjectiveInferenceFunc infers implicit user objectives from recent patterns.
type ObjectiveInferenceFunc func(ctx context.Context) error

// inferredObjective is a single objective from the DTC response.
type inferredObjective struct {
	Objective string `json:"objective"`
	Evidence  string `json:"evidence"`
	Priority  string `json:"priority"`
}

type objectiveInferenceResult struct {
	Objectives []inferredObjective `json:"objectives"`
}

// runObjectiveInferenceIfReady checks conditions and fires objective inference.
func (e *Engine) runObjectiveInferenceIfReady() {
	if e.Cognitive.ObjectiveInferenceFunc == nil {
		return
	}

	// Cooldown check.
	cooldown := e.ObjectiveInferenceCooldown
	if cooldown == 0 {
		cooldown = defaultObjectiveInferenceCooldown
	}
	if time.Since(e.lastObjectiveInferenceRun) < cooldown {
		return
	}

	// User activity check.
	lastActivity := e.SM.LastUserActivity()
	if lastActivity.IsZero() || time.Since(lastActivity) > objectiveInferenceMaxInactive {
		return
	}

	// Check memory volume: at least N high-salience memories in 48h.
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()
	count, err := memory.CountRecentMemories(ctx, e.DB, 48, memory.SalienceLow)
	if err != nil || count < objectiveInferenceMinMemories {
		return
	}

	logger.Log.Info("[objectives] conditions met, running objective inference")
	inferCtx, inferCancel := context.WithTimeout(context.Background(), timeouts.Synthesis)
	defer inferCancel()

	if err := e.Cognitive.ObjectiveInferenceFunc(inferCtx); err != nil {
		logger.Log.Warnf("[objectives] inference failed: %v", err)
		return
	}

	e.lastObjectiveInferenceRun = time.Now()
}

// RunObjectiveInference performs objective inference using the deep thinker. It queries
// recent memories + identity profile, calls DTC, and saves novel inferred objectives
// to the objectives table.
func RunObjectiveInference(ctx context.Context, db *pgxpool.Pool, dtcComplete func(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error)) error {
	// Gather recent high-salience memories.
	mems, err := memory.QueryRecentMemories(ctx, db, 48, memory.SalienceLow, 30)
	if err != nil {
		return fmt.Errorf("query recent memories: %w", err)
	}
	summaries := make([]string, len(mems))
	for i, m := range mems {
		summaries[i] = m.Summary
	}

	if len(summaries) < objectiveInferenceMinMemories {
		logger.Log.Debug("[objectives] insufficient memories after query, skipping")
		return nil
	}

	// Get identity profile for context.
	profile, err := memory.GetIdentityProfile(ctx, db)
	if err != nil {
		profile = "{}"
	}

	// Build user content.
	var b strings.Builder
	b.WriteString("## Identity Profile\n")
	b.WriteString(profile)
	b.WriteString("\n\n## Recent Memories (last 48h, high-salience)\n")
	for i, s := range summaries {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}

	// Call DTC with thinking enabled for reasoning.
	raw, err := dtcComplete(ctx, strings.TrimSpace(prompts.ObjectiveInference), b.String(), tokens.ObjectiveInference)
	if err != nil {
		return fmt.Errorf("DTC objective inference: %w", err)
	}

	result, err := textutil.ParseLLMJSON[objectiveInferenceResult](raw)
	if err != nil {
		return fmt.Errorf("parse objective inference: %w", err)
	}

	if len(result.Objectives) == 0 {
		logger.Log.Info("[objectives] no new objectives inferred")
		return nil
	}

	saved := 0
	for _, g := range result.Objectives {
		if strings.TrimSpace(g.Objective) == "" {
			continue
		}

		// Dedup via ILIKE on existing objectives.
		similar, _ := objectives.FindSimilar(ctx, db, g.Objective)
		if len(similar) > 0 {
			logger.Log.Infof("[objectives] skipping duplicate objective: %s", textutil.Truncate(g.Objective, 80))
			continue
		}

		priority := g.Priority
		switch priority {
		case "high", "medium", "low":
			// valid
		default:
			priority = "medium"
		}

		id, err := objectives.Create(ctx, db, g.Objective, priority, "inferred")
		if err != nil {
			logger.Log.Warnf("[objectives] failed to create objective: %v", err)
			continue
		}
		saved++
		logger.Log.Infof("[objectives] saved inferred objective #%d (%s): %s", id, priority, textutil.Truncate(g.Objective, 80))
	}

	logger.Log.Infof("[objectives] inference complete: %d objectives inferred, %d saved", len(result.Objectives), saved)
	return nil
}
