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
//     monitor_inbox routine.
//   - With arguments: pass-through Gmail search with optional time filters.
//     Used for manual queries like "find emails from Mary."
func NewSearchEmail(svc *gm.Service, pool *pgxpool.Pool) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchEmailArgs
		if args != nil && len(args) > 2 {
			if err := json.Unmarshal(args, &a); err != nil {
				return fmt.Sprintf("invalid arguments: %v", err), nil
			}
		}

		// No-arg mode: dedup check for new emails.
		if a.Query == "" && a.TimeMin == "" && a.TimeMax == "" {
			return checkNewEmails(ctx, svc, pool)
		}

		// Search mode: pass-through to Gmail.
		return searchGmail(ctx, svc, a)
	}
}

// checkNewEmails fetches recent emails, filters out already-seen IDs, and
// records new ones. Returns only unseen emails.
func checkNewEmails(ctx context.Context, svc *gm.Service, pool *pgxpool.Pool) (string, error) {
	emails, err := gmail.FetchEmails(svc, "newer_than:1h", 20)
	if err != nil {
		return fmt.Sprintf("Failed to fetch emails: %v", err), nil
	}
	if len(emails) == 0 {
		return "No emails in the last hour.", nil
	}

	var newEmails []gmail.Email
	for _, e := range emails {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM processed_emails WHERE message_id = $1)`,
			e.ID).Scan(&exists)
		if err != nil {
			logger.Log.Warnf("[search_email] dedup lookup failed for %s: %v", e.ID, err)
			newEmails = append(newEmails, e)
			continue
		}
		if !exists {
			newEmails = append(newEmails, e)
		}
	}

	if len(newEmails) == 0 {
		return "No new emails since last check.", nil
	}

	// Cap batch size to avoid blowing up the context window.
	// Unseen emails beyond the cap stay unrecorded and get picked up next cycle.
	const maxBatch = 5
	batch := newEmails
	if len(batch) > maxBatch {
		batch = batch[:maxBatch]
	}

	for _, e := range batch {
		if _, err := pool.Exec(ctx,
			`INSERT INTO processed_emails (message_id) VALUES ($1) ON CONFLICT DO NOTHING`,
			e.ID); err != nil {
			logger.Log.Warnf("[search_email] failed to record %s as processed: %v", e.ID, err)
		}
	}

	header := fmt.Sprintf("%d new email(s):", len(batch))
	if len(newEmails) > maxBatch {
		header = fmt.Sprintf("%d new email(s) (%d more next cycle):", len(batch), len(newEmails)-maxBatch)
	}
	return formatEmails(batch, header), nil
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
		if t, err := time.Parse(time.RFC3339, a.TimeMin); err == nil {
			queryParts = append(queryParts, fmt.Sprintf("after:%d", t.Unix()))
		}
	}
	if a.TimeMax != "" {
		if t, err := time.Parse(time.RFC3339, a.TimeMax); err == nil {
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
