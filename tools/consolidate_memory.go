package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
)

const salienceThreshold = 8

// consolidateOpts configures a consolidation run.
type consolidateOpts struct {
	SalienceThreshold int
	MemoryLimit       int
	Timeout           time.Duration
}

// consolidateCore is the shared consolidation pipeline: query high-salience
// memories → read profile → build prompt → call deep thinker → strip fences →
// validate JSON → write profile. Returns the count of memories synthesized.
func consolidateCore(ctx context.Context, pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, opts consolidateOpts) (int, error) {
	memories, err := QueryHighSalienceMemories(ctx, pool, opts.SalienceThreshold, opts.MemoryLimit)
	if err != nil {
		return 0, fmt.Errorf("query high-salience memories: %w", err)
	}
	if len(memories) == 0 {
		return 0, nil
	}

	currentProfile, err := memory.GetIdentityProfile(ctx, pool)
	if err != nil {
		logger.Log.Warnf("[consolidate] failed to read profile from DB: %v", err)
		currentProfile = "{}"
	}

	var b strings.Builder
	b.WriteString("CURRENT PROFILE:\n")
	b.WriteString(currentProfile)
	b.WriteString("\n\nHIGH-SALIENCE MEMORIES:\n")
	fmt.Fprintf(&b, "(Current time: %s)\n\n", time.Now().Format(time.RFC3339))
	for i, m := range memories {
		fmt.Fprintf(&b, "%d. %s\n", i+1, m)
	}

	updatedProfile, err := dtc.CompleteNoThink(ctx, strings.TrimSpace(prompts.Consolidation), b.String(), 4096)
	if err != nil {
		return 0, fmt.Errorf("consolidation request: %w", err)
	}

	updatedProfile = textutil.StripCodeFences(updatedProfile)
	var check json.RawMessage
	if err := json.Unmarshal([]byte(updatedProfile), &check); err != nil {
		return 0, fmt.Errorf("invalid JSON from deep thinker: %w", err)
	}

	if err := memory.WriteIdentityProfile(ctx, pool, embedEndpoint, embedModel, updatedProfile); err != nil {
		return 0, fmt.Errorf("write updated profile: %w", err)
	}

	return len(memories), nil
}

// NewConsolidateMemory returns a ToolFunc that triggers the memory consolidation
// pipeline: query high-salience memories from pgvector, read the current identity
// profile from the database, send both to the Deep Thinker, and write the updated
// profile back as an identity memory row.
func NewConsolidateMemory(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, memoryLimit int) ToolFunc {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		if pool == nil {
			return "Memory consolidation unavailable: no database configured.", nil
		}
		if dtc == nil {
			return "Memory consolidation unavailable: no deep thinker configured.", nil
		}

		n, err := consolidateCore(ctx, pool, dtc, embedEndpoint, embedModel, consolidateOpts{
			SalienceThreshold: salienceThreshold,
			MemoryLimit:       memoryLimit,
		})
		if err != nil {
			return fmt.Sprintf("Consolidation failed: %v", err), nil
		}
		if n == 0 {
			return "No high-salience memories (score 8+) found in the last 24 hours. No consolidation needed.", nil
		}

		logger.Log.Infof("[consolidate] profile updated from %d high-salience memories", n)
		return fmt.Sprintf("Memory consolidation complete. Synthesized %d high-salience memories into core profile.", n), nil
	}
}

// RunInitialConsolidation runs a one-shot memory consolidation at startup.
// It is intended to be called as a fire-and-forget goroutine so the identity
// profile exists as early as possible.
func RunInitialConsolidation(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, memoryLimit int) {
	logger.Log.Info("[consolidate] running initial profile consolidation on startup")

	ctx, cancel := context.WithTimeout(context.Background(), TimeoutInitConsolidation)
	defer cancel()

	// Use a lower threshold (5) than the regular consolidation (8) so the
	// initial profile can incorporate all available memories, including
	// freshly backfilled emails and conversations.
	n, err := consolidateCore(ctx, pool, dtc, embedEndpoint, embedModel, consolidateOpts{
		SalienceThreshold: 5,
		MemoryLimit:       memoryLimit,
		Timeout:           TimeoutInitConsolidation,
	})
	if err != nil {
		logger.Log.Errorf("[consolidate] startup: %v", err)
		return
	}
	if n == 0 {
		logger.Log.Info("[consolidate] startup: no memories found, skipping")
		return
	}

	logger.Log.Infof("[consolidate] startup: profile created/updated from %d high-salience memories", n)
}
