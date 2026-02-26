package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	gm "google.golang.org/api/gmail/v1"

	"sokratos/gmail"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
)

type checkEmailArgs struct {
	MaxResults int64 `json:"max_results"`
}

// NewCheckEmail returns a ToolFunc that fetches recent emails, skips any
// already processed (by Gmail message ID), triages new ones through the
// Deep Thinker, and saves all of them to memory with full-text embedding.
func NewCheckEmail(svc *gm.Service, pool *pgxpool.Pool, embedEndpoint, embedModel string, dtc *DeepThinkerClient, granite memory.GraniteFunc) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		maxResults := int64(10)
		if args != nil && len(args) > 2 {
			var a checkEmailArgs
			if err := json.Unmarshal(args, &a); err == nil && a.MaxResults > 0 {
				maxResults = a.MaxResults
			}
		}

		emails, err := gmail.FetchEmails(svc, "newer_than:1h", maxResults)
		if err != nil {
			return fmt.Sprintf("Failed to fetch unread emails: %v", err), nil
		}

		if len(emails) == 0 {
			return "No emails in the last hour.", nil
		}

		// Filter out already-processed emails.
		emails, err = FilterProcessed(ctx, pool, ProcessedEmails, emails, func(e gmail.Email) string { return e.ID })
		if err != nil {
			logger.Log.Warnf("[check_email] dedup check failed, processing all: %v", err)
		}

		if len(emails) == 0 {
			return "No new emails since last check.", nil
		}

		items := make([]CheckItem, len(emails))
		for i, e := range emails {
			e := e // capture for closure
			var srcDate *time.Time
			if !e.Date.IsZero() {
				d := e.Date
				srcDate = &d
			}
			items[i] = CheckItem{
				ID:           e.ID,
				DisplayLabel: fmt.Sprintf("%q from %s", e.Subject, e.From),
				TriageText:   gmail.FormatEmailSummary(e),
				EmbeddingText: func(triageLine string) string {
					return gmail.FormatForEmbedding(e, triageLine)
				},
				SourceDate: srcDate,
			}
		}

		return ProcessCheckItems(ctx, CheckConfig{
			Pool:           pool,
			EmbedEndpoint:  embedEndpoint,
			EmbedModel:     embedModel,
			DTC:            dtc,
			GraniteFunc:    granite,
			LogPrefix:      "[check_email]",
			DomainTag:      "email",
			TriagePrompt:   strings.TrimSpace(prompts.EmailTriage),
			ProcessedTable: ProcessedEmails,
		}, items), nil
	}
}

