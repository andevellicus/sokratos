package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/memory"
)

// CheckItem represents one item (email or calendar event) to be triaged and
// saved to memory.
type CheckItem struct {
	ID            string
	DisplayLabel  string                         // e.g. `"subject" from sender` — used in failure logs
	TriageText    string                         // pre-formatted text for triage
	EmbeddingText func(triageLine string) string // builds full text for embedding
	SourceDate    *time.Time                     // original date of the item (email date, event start)
}

// CheckConfig holds the shared dependencies for a check pipeline.
type CheckConfig struct {
	Pool           *pgxpool.Pool
	EmbedEndpoint  string
	EmbedModel     string
	DTC            *DeepThinkerClient
	GraniteFunc    memory.GraniteFunc // optional: enables contradiction detection + quality scoring
	LogPrefix      string             // "[check_email]"
	DomainTag      string             // "email"
	TriagePrompt   string             // prompts.EmailTriage (already trimmed)
	ProcessedTable ProcessedTable     // ProcessedEmails
}

// ProcessCheckItems triages each item, records it as processed, saves to
// memory, and returns a formatted summary string.
func ProcessCheckItems(ctx context.Context, cfg CheckConfig, items []CheckItem) string {
	var important, routine, failed int
	var summaries []string

	for _, item := range items {
		result, triageErr := cfg.DTC.TriageItem(ctx, cfg.TriagePrompt, item.TriageText, 4000)

		// Record as processed regardless of triage outcome.
		if recErr := RecordProcessedID(ctx, cfg.Pool, cfg.ProcessedTable, item.ID); recErr != nil {
			logger.Log.Warnf("%s failed to record %s as processed: %v", cfg.LogPrefix, item.ID, recErr)
		}

		if triageErr != nil {
			logger.Log.Errorf("%s triage failed for %s: %v", cfg.LogPrefix, item.DisplayLabel, triageErr)
			failed++
			summaries = append(summaries, fmt.Sprintf("- [triage failed] %s", item.DisplayLabel))
			continue
		}

		if result.Save != nil {
			logger.Log.Debugf("%s triage save=%v for %s (score=%.0f)", cfg.LogPrefix, *result.Save, item.DisplayLabel, result.SalienceScore)
		}

		triageLine := fmt.Sprintf("Triage: score=%.0f, category=%s\nSummary: %s", result.SalienceScore, result.Category, result.Summary)
		embeddingText := item.EmbeddingText(triageLine)

		tags := append([]string{cfg.DomainTag}, result.Tags...)
		if cfg.GraniteFunc != nil {
			req := memory.MemoryWriteRequest{
				Summary:       embeddingText,
				Tags:          tags,
				Salience:      result.SalienceScore,
				MemoryType:    cfg.DomainTag,
				Source:        cfg.DomainTag,
				SourceDate:    item.SourceDate,
				EmbedEndpoint: cfg.EmbedEndpoint,
				EmbedModel:    cfg.EmbedModel,
			}
			go func() {
				saveCtx, saveCancel := context.WithTimeout(context.Background(), TimeoutMemorySave)
				defer saveCancel()
				if _, err := memory.CheckAndWriteWithContradiction(saveCtx, cfg.Pool, req, cfg.GraniteFunc); err != nil {
					logger.Log.Warnf("%s contradiction-checked save failed: %v", cfg.LogPrefix, err)
				}
			}()
		} else {
			memory.SaveToMemoryWithSalienceAsync(cfg.Pool, cfg.EmbedEndpoint, cfg.EmbedModel, embeddingText, result.SalienceScore, tags, cfg.DomainTag, item.SourceDate)
		}

		if result.SalienceScore >= 7 {
			important++
		} else {
			routine++
		}
		summaries = append(summaries, fmt.Sprintf("- [score %.0f, saved] %s", result.SalienceScore, result.Summary))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d new item(s) processed: %d important, %d routine (all saved to memory)", len(items), important, routine)
	if failed > 0 {
		fmt.Fprintf(&b, ", %d triage failed", failed)
	}
	b.WriteString("\n\n")
	for _, s := range summaries {
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}
