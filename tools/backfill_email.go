package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	gm "google.golang.org/api/gmail/v1"

	"sokratos/gmail"
	"sokratos/logger"
	"sokratos/prompts"
)

// BackfillBase holds the shared dependencies for all backfill pipelines.
type BackfillBase struct {
	Pool          *pgxpool.Pool
	EmbedEndpoint string
	EmbedModel    string
	DTC           *DeepThinkerClient
	BackfillDays  int // 0 → use pipeline-specific default
}

// BackfillConfig holds everything RunEmailBackfill needs.
type BackfillConfig struct {
	BackfillBase
	GmailService *gm.Service
}

// RunEmailBackfill ingests historical emails into the memory store.
func RunEmailBackfill(cfg BackfillConfig) {
	days := cfg.BackfillDays
	if days <= 0 {
		days = 30
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	query := fmt.Sprintf("after:%s", cutoff.Format("2006/01/02"))
	logger.Log.Infof("[backfill] starting email backfill: query=%q (last %d days)", query, days)

	RunBackfillPipeline(BackfillPipelineConfig{
		Pool:           cfg.Pool,
		EmbedEndpoint:  cfg.EmbedEndpoint,
		EmbedModel:     cfg.EmbedModel,
		DTC:            cfg.DTC,
		LogPrefix:      "[backfill]",
		Kind:           "email",
		DomainTag:      "email",
		TriagePrompt:   strings.TrimSpace(prompts.EmailTriage),
		ProcessedTable: ProcessedEmails,
		BackfillKey:    query,
		ThrottleItem:   2 * time.Second,
		ThrottlePage:   5 * time.Second,
		FetchPage: func(ctx context.Context, pageToken string) ([]string, string, error) {
			req := cfg.GmailService.Users.Messages.List("me").Q(query).MaxResults(25)
			if pageToken != "" {
				req = req.PageToken(pageToken)
			}
			list, err := req.Do()
			if err != nil {
				return nil, "", err
			}
			ids := make([]string, len(list.Messages))
			for i, m := range list.Messages {
				ids[i] = m.Id
			}
			return ids, list.NextPageToken, nil
		},
		BuildItem: func(ctx context.Context, id string) (*BackfillItem, error) {
			msg, err := cfg.GmailService.Users.Messages.Get("me", id).Format("full").Do()
			if err != nil {
				return nil, err
			}
			e := gmail.ParseMessage(msg)
			var srcDate *time.Time
			if !e.Date.IsZero() {
				d := e.Date
				srcDate = &d
			}
			return &BackfillItem{
				ID:           e.ID,
				DisplayLabel: fmt.Sprintf("%q from %s", e.Subject, e.From),
				TriageText:   gmail.FormatEmailSummary(e),
				EmbeddingText: func(triageLine string) string {
					return gmail.FormatForEmbedding(e, triageLine)
				},
				SourceDate: srcDate,
			}, nil
		},
	})
}
