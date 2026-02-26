package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/memory"
)

// BackfillItem represents one fully-resolved item ready for triage and save.
type BackfillItem struct {
	ID            string
	DisplayLabel  string
	TriageText    string
	EmbeddingText func(triageLine string) string
	SourceDate    *time.Time // original date of the source item
}

// BackfillPipelineConfig holds the shared dependencies for a backfill pipeline.
type BackfillPipelineConfig struct {
	Pool                       *pgxpool.Pool
	EmbedEndpoint, EmbedModel  string
	DTC                        *DeepThinkerClient
	LogPrefix                  string         // "[backfill]"
	Kind                       string         // "email" / "calendar"
	DomainTag                  string         // "email" / "calendar"
	TriagePrompt               string         // already trimmed
	ProcessedTable             ProcessedTable
	BackfillKey                string         // unique key for backfill_runs dedup
	ThrottleItem               time.Duration  // 2s for email, 1s for calendar
	ThrottlePage               time.Duration  // 5s for email, 3s for calendar
	// FetchPage returns the IDs on this page and the next page token.
	FetchPage func(ctx context.Context, pageToken string) (ids []string, nextToken string, err error)
	// BuildItem resolves a single ID into a full BackfillItem (e.g. fetching
	// the full Gmail message). Return nil to skip the item.
	BuildItem func(ctx context.Context, id string) (*BackfillItem, error)
}

// RunBackfillPipeline is the shared pagination+triage+save loop for both
// email and calendar backfill.
func RunBackfillPipeline(cfg BackfillPipelineConfig) {
	ctx := context.Background()

	// Skip if backfill already completed for this key.
	var done bool
	_ = cfg.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM backfill_runs WHERE kind = $1 AND query = $2)`,
		cfg.Kind, cfg.BackfillKey,
	).Scan(&done)
	if done {
		logger.Log.Infof("%s already completed for %q — skipping", cfg.LogPrefix, cfg.BackfillKey)
		return
	}

	pageToken := ""
	totalProcessed := 0
	totalSkipped := 0
	page := 0

	for {
		page++

		ids, nextToken, err := cfg.FetchPage(ctx, pageToken)
		if err != nil {
			logger.Log.Errorf("%s list failed on page %d: %v", cfg.LogPrefix, page, err)
			break
		}

		if len(ids) == 0 {
			break
		}

		// Bulk dedup.
		seen, err := LookupProcessedIDs(ctx, cfg.Pool, cfg.ProcessedTable, ids)
		if err != nil {
			logger.Log.Warnf("%s dedup check failed on page %d, processing all: %v", cfg.LogPrefix, page, err)
			seen = make(map[string]struct{})
		}

		pageProcessed := 0
		pageSkipped := 0

		for _, id := range ids {
			if _, ok := seen[id]; ok {
				pageSkipped++
				continue
			}

			item, buildErr := cfg.BuildItem(ctx, id)
			if buildErr != nil {
				logger.Log.Warnf("%s failed to fetch item %s: %v", cfg.LogPrefix, id, buildErr)
				continue
			}
			if item == nil {
				pageSkipped++
				continue
			}

			result, triageErr := cfg.DTC.TriageItem(ctx, cfg.TriagePrompt, item.TriageText, 4000)
			if triageErr != nil {
				logger.Log.Warnf("%s triage failed for %s: %v", cfg.LogPrefix, item.DisplayLabel, triageErr)
				time.Sleep(cfg.ThrottleItem)
				continue
			}

			triageLine := fmt.Sprintf("Triage: score=%.0f, category=%s\nSummary: %s", result.SalienceScore, result.Category, result.Summary)
			embeddingText := item.EmbeddingText(triageLine)

			tags := append([]string{cfg.DomainTag}, result.Tags...)
			if err := memory.SaveToMemoryWithSalience(ctx, cfg.Pool, cfg.EmbedEndpoint, cfg.EmbedModel, embeddingText, result.SalienceScore, tags, "backfill", item.SourceDate); err != nil {
				logger.Log.Warnf("%s failed to save %s to memory: %v", cfg.LogPrefix, item.DisplayLabel, err)
				_ = RecordProcessedID(ctx, cfg.Pool, cfg.ProcessedTable, item.ID)
				time.Sleep(cfg.ThrottleItem)
				continue
			}

			if recErr := RecordProcessedID(ctx, cfg.Pool, cfg.ProcessedTable, item.ID); recErr != nil {
				logger.Log.Warnf("%s failed to record %s as processed: %v", cfg.LogPrefix, item.ID, recErr)
			}

			pageProcessed++
			time.Sleep(cfg.ThrottleItem)
		}

		totalProcessed += pageProcessed
		totalSkipped += pageSkipped
		logger.Log.Infof("%s page %d: processed=%d, skipped=%d (total: processed=%d, skipped=%d)",
			cfg.LogPrefix, page, pageProcessed, pageSkipped, totalProcessed, totalSkipped)

		if nextToken == "" {
			break
		}
		pageToken = nextToken

		if pageProcessed == 0 {
			continue
		}

		time.Sleep(cfg.ThrottlePage)
	}

	// Record completion so we don't re-run on next startup.
	_, _ = cfg.Pool.Exec(ctx,
		`INSERT INTO backfill_runs (kind, query) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		cfg.Kind, cfg.BackfillKey,
	)

	logger.Log.Infof("%s completed: %d processed, %d skipped (already in DB)", cfg.LogPrefix, totalProcessed, totalSkipped)
}

