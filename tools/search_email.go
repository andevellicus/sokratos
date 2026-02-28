package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	gm "google.golang.org/api/gmail/v1"

	"sokratos/gmail"
	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/timefmt"
)

type searchEmailArgs struct {
	Query      string  `json:"query"`
	MaxResults float64 `json:"max_results"`
	TimeMin    string  `json:"time_min"`
	TimeMax    string  `json:"time_max"`
}

// NewSearchEmail returns a ToolFunc that searches Gmail.
//
// Two modes:
//   - No arguments: fetches newer_than:1h, skips already-seen message IDs
//     (via processed_emails table), returns only new emails. Used by the
//     monitor_inbox routine. When triageCfg is non-nil, all new emails are
//     triaged and saved to memory in parallel background goroutines.
//   - With arguments: pass-through Gmail search with optional time filters.
//     Used for manual queries like "find emails from Mary."
func NewSearchEmail(svc *gm.Service, pool *pgxpool.Pool, triageCfg *TriageConfig, lookback string, displayBatch int) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchEmailArgs
		if args != nil && len(args) > 2 {
			if err := json.Unmarshal(args, &a); err != nil {
				return fmt.Sprintf("invalid arguments: %v", err), nil
			}
		}

		// No-arg mode: dedup check for new emails.
		if a.Query == "" && a.TimeMin == "" && a.TimeMax == "" {
			return checkNewEmails(ctx, svc, pool, triageCfg, lookback, displayBatch)
		}

		// Search mode: pass-through to Gmail.
		return searchGmail(ctx, svc, a)
	}
}

// checkNewEmails fetches recent emails, filters out already-seen IDs, records
// ALL new ones as processed, and triages each in a background goroutine (when
// triageCfg is non-nil). Returns only the first maxBatch emails to the
// orchestrator for immediate action — context window protection.
func checkNewEmails(ctx context.Context, svc *gm.Service, pool *pgxpool.Pool, triageCfg *TriageConfig, lookback string, displayBatch int) (string, error) {
	emails, err := gmail.FetchEmails(svc, lookback, 20)
	if err != nil {
		return fmt.Sprintf("Failed to fetch emails: %v", err), nil
	}
	if len(emails) == 0 {
		return "No emails in the last hour.", nil
	}

	tracker := &ProcessedTracker{Pool: pool, Table: "processed_emails", IDColumn: "message_id"}
	emailIDs := make([]string, len(emails))
	for i, e := range emails {
		emailIDs[i] = e.ID
	}
	newIDs := tracker.FilterNew(ctx, emailIDs)
	newIDSet := make(map[string]bool, len(newIDs))
	for _, id := range newIDs {
		newIDSet[id] = true
	}
	var newEmails []gmail.Email
	for _, e := range emails {
		if newIDSet[e.ID] {
			newEmails = append(newEmails, e)
		}
	}

	if len(newEmails) == 0 {
		return "No new emails since last check.", nil
	}

	// Record ALL new emails as processed and triage each in parallel.
	markIDs := make([]string, len(newEmails))
	for i, e := range newEmails {
		markIDs[i] = e.ID
	}
	tracker.MarkProcessed(ctx, markIDs)
	for _, e := range newEmails {
		if triageCfg != nil {
			triageAndSaveEmailAsync(*triageCfg, e)
		}
	}

	// Return only the capped batch to orchestrator for immediate action.
	batch := newEmails
	if len(batch) > displayBatch {
		batch = batch[:displayBatch]
	}

	header := fmt.Sprintf("%d new email(s):", len(batch))
	if len(newEmails) > displayBatch {
		header = fmt.Sprintf("%d new email(s) (%d more triaged in background):", len(batch), len(newEmails)-displayBatch)
	}
	return formatEmails(batch, header), nil
}

// triageAndSaveEmailAsync sends an email for triage scoring, then saves it to
// memory if it meets the salience threshold. Runs as a fire-and-forget goroutine.
func triageAndSaveEmailAsync(cfg TriageConfig, email gmail.Email) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
		defer cancel()

		formatted := gmail.FormatEmailSummary(email)
		sourceDate := email.Date

		err := triageAndSave(ctx, cfg, TriageSaveRequest{
			TriagePrompt:  strings.TrimSpace(prompts.EmailTriage),
			TriageInput:   formatted,
			SourceContent: formatted,
			SourceLabel:   "Source email",
			DomainTag:     "email",
			MemoryType:    "email",
			Source:        "email",
			SourceDate:    &sourceDate,
			MaxTriageLen:  4000,
			ShouldSave: func(r *triageResult) bool {
				if r.Save != nil && !*r.Save {
					return false
				}
				return r.SalienceScore >= 1
			},
		})
		if err != nil {
			logger.Log.Warnf("[email_triage] failed: %v", err)
			if cfg.RetryQueue != nil {
				EnqueueEmailTriage(cfg.RetryQueue, cfg, formatted, formatted)
			}
		}
	}()
}

// searchGmail performs a direct Gmail API search with query and optional time filters.
func searchGmail(ctx context.Context, svc *gm.Service, a searchEmailArgs) (string, error) {
	maxResults := int64(10)
	if a.MaxResults > 0 {
		maxResults = int64(a.MaxResults)
	}

	// Strip temporal-only queries when time bounds already express the intent.
	// Gmail search treats the query as full-text — "today" would only match
	// emails containing the literal word "today".
	query := a.Query
	if (a.TimeMin != "" || a.TimeMax != "") && temporalOnlyQuery(query) {
		logger.Log.Infof("[search_email] stripped temporal-only query %q (time bounds already set)", query)
		query = ""
	}

	var queryParts []string
	if query != "" {
		queryParts = append(queryParts, query)
	}
	if a.TimeMin != "" {
		if t, err := timefmt.ParseISO8601(a.TimeMin); err == nil {
			queryParts = append(queryParts, fmt.Sprintf("after:%d", t.Unix()))
		}
	}
	if a.TimeMax != "" {
		if t, err := timefmt.ParseISO8601(a.TimeMax); err == nil {
			queryParts = append(queryParts, fmt.Sprintf("before:%d", t.Unix()))
		}
	}

	finalQuery := strings.Join(queryParts, " ")

	emails, err := gmail.FetchEmails(svc, finalQuery, maxResults)
	if err != nil {
		logger.Log.Errorf("[search_email] failed: %v", err)
		return fmt.Sprintf("Failed to search email: %v", err), nil
	}
	if len(emails) == 0 {
		return "No emails found matching your query.", nil
	}

	logger.Log.Infof("[search_email] %d results found for %q", len(emails), finalQuery)
	return formatEmails(emails, fmt.Sprintf("Found %d email(s):", len(emails))), nil
}

func formatEmails(emails []gmail.Email, header string) string {
	var b strings.Builder
	b.WriteString(header)
	for i, e := range emails {
		fmt.Fprintf(&b, "\n\n--- Email %d ---\n%s", i+1, gmail.FormatEmailSummary(e))
	}
	return b.String()
}
