package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timeouts"
)

const (
	goalInferenceCooldown    = 12 * time.Hour
	goalInferenceMaxInactive = 24 * time.Hour
	goalInferenceMinMemories = 10
	goalSimilarityThreshold  = 0.2 // cosine distance below this = duplicate
)

// GoalInferenceFunc infers implicit user goals from recent patterns.
type GoalInferenceFunc func(ctx context.Context) error

// inferredGoal is a single goal from the DTC response.
type inferredGoal struct {
	Goal     string `json:"goal"`
	Evidence string `json:"evidence"`
	Priority string `json:"priority"`
}

type goalInferenceResult struct {
	Goals []inferredGoal `json:"goals"`
}

// runGoalInferenceIfReady checks conditions and fires goal inference.
func (e *Engine) runGoalInferenceIfReady() {
	if e.Cognitive.GoalInferenceFunc == nil {
		return
	}

	// Cooldown check.
	if time.Since(e.lastGoalInferenceRun) < goalInferenceCooldown {
		return
	}

	// User activity check.
	lastActivity := e.SM.LastUserActivity()
	if lastActivity.IsZero() || time.Since(lastActivity) > goalInferenceMaxInactive {
		return
	}

	// Check memory volume: at least N high-salience memories in 48h.
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()
	count, err := memory.CountRecentMemories(ctx, e.DB, 48, memory.SalienceLow)
	if err != nil || count < goalInferenceMinMemories {
		return
	}

	logger.Log.Info("[goals] conditions met, running goal inference")
	inferCtx, inferCancel := context.WithTimeout(context.Background(), timeouts.Synthesis)
	defer inferCancel()

	if err := e.Cognitive.GoalInferenceFunc(inferCtx); err != nil {
		logger.Log.Warnf("[goals] inference failed: %v", err)
		return
	}

	e.lastGoalInferenceRun = time.Now()
}

// RunGoalInference performs goal inference using the deep thinker. It queries
// recent memories + identity profile, calls DTC, and saves novel inferred goals.
func RunGoalInference(ctx context.Context, db *pgxpool.Pool, dtcComplete func(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error), embedEndpoint, embedModel string, grammarFn memory.GrammarSubagentFunc) error {
	// Gather recent high-salience memories.
	mems, err := memory.QueryRecentMemories(ctx, db, 48, memory.SalienceLow, 30)
	if err != nil {
		return fmt.Errorf("query recent memories: %w", err)
	}
	summaries := make([]string, len(mems))
	for i, m := range mems {
		summaries[i] = m.Summary
	}

	if len(summaries) < goalInferenceMinMemories {
		logger.Log.Debug("[goals] insufficient memories after query, skipping")
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
	raw, err := dtcComplete(ctx, strings.TrimSpace(prompts.GoalInference), b.String(), 2048)
	if err != nil {
		return fmt.Errorf("DTC goal inference: %w", err)
	}

	raw = textutil.CleanLLMJSON(raw)

	var result goalInferenceResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return fmt.Errorf("parse goal inference: %w (raw: %s)", err, textutil.Truncate(raw, 200))
	}

	if len(result.Goals) == 0 {
		logger.Log.Info("[goals] no new goals inferred")
		return nil
	}

	saved := 0
	for _, g := range result.Goals {
		if strings.TrimSpace(g.Goal) == "" {
			continue
		}

		// Check for duplicate goals via embedding similarity.
		if isDuplicateGoal(ctx, db, embedEndpoint, embedModel, g.Goal) {
			logger.Log.Infof("[goals] skipping duplicate goal: %s", textutil.Truncate(g.Goal, 80))
			continue
		}

		salience := 8.0
		if g.Priority == "low" {
			salience = 6.0
		} else if g.Priority == "medium" {
			salience = 7.0
		}

		goalText := fmt.Sprintf("[Inferred Goal] %s\nEvidence: %s\nPriority: %s", g.Goal, g.Evidence, g.Priority)
		memory.SaveToMemoryWithSalienceAsync(db, memory.MemoryWriteRequest{
			Summary:       goalText,
			Tags:          []string{"goal", "inferred"},
			Salience:      salience,
			MemoryType:    "goal",
			Source:        "goal_inference",
			EmbedEndpoint: embedEndpoint,
			EmbedModel:    embedModel,
		}, grammarFn, nil)
		saved++
		logger.Log.Infof("[goals] saved inferred goal (salience=%.0f): %s", salience, textutil.Truncate(g.Goal, 80))
	}

	logger.Log.Infof("[goals] inference complete: %d goals inferred, %d saved", len(result.Goals), saved)
	return nil
}

// isDuplicateGoal checks if a similar goal already exists in memory.
func isDuplicateGoal(ctx context.Context, db *pgxpool.Pool, embedEndpoint, embedModel, goal string) bool {
	embedding, err := memory.GetEmbedding(ctx, embedEndpoint, embedModel, goal)
	if err != nil {
		return false // fail open — save the goal
	}

	var dist float64
	err = db.QueryRow(ctx,
		`SELECT MIN(embedding <=> $1) FROM memories
		 WHERE memory_type = 'goal'
		   AND superseded_by IS NULL
		   AND created_at >= NOW() - INTERVAL '30 days'`,
		pgvector.NewVector(embedding),
	).Scan(&dist)
	if err != nil {
		return false
	}

	return dist < goalSimilarityThreshold
}
