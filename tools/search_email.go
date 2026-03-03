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
	"sokratos/pipelines"
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
//   - No arguments: fetches all emails (inbox, sent, archived — excludes
//     trash/spam) since the last successful check. The check timestamp is
//     persisted in the processed_emails_meta table so the window always
//     matches the actual interval, regardless of routine config. On first
//     run, defaults to 1h lookback. When triageCfg is non-nil, all new
//     emails are triaged and saved to memory in parallel background goroutines.
//   - With arguments: pass-through Gmail search with optional time filters.
//     Used for manual queries like "find emails from Mary."
func NewSearchEmail(svc *gm.Service, pool *pgxpool.Pool, triageCfg *pipelines.TriageConfig, displayBatch int) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchEmailArgs
		if len(args) > 2 {
			if err := json.Unmarshal(args, &a); err != nil {
				return fmt.Sprintf("invalid arguments: %v", err), nil
			}
		}

		// No-arg mode: dedup check for new emails.
		if a.Query == "" && a.TimeMin == "" && a.TimeMax == "" {
			return checkNewEmails(ctx, svc, pool, triageCfg, displayBatch)
		}

		// Search mode: pass-through to Gmail.
		return searchGmail(svc, a)
	}
}

// lastEmailCheck reads the last successful check timestamp from the DB.
// Returns zero time if no previous check exists.
func lastEmailCheck(ctx context.Context, pool *pgxpool.Pool) time.Time {
	if pool == nil {
		return time.Time{}
	}
	var t time.Time
	err := pool.QueryRow(ctx,
		`SELECT checked_at FROM processed_emails_meta WHERE key = 'last_check'`).Scan(&t)
	if err != nil {
		return time.Time{}
	}
	return t
}

// saveEmailCheck persists the check timestamp.
func saveEmailCheck(ctx context.Context, pool *pgxpool.Pool, t time.Time) {
	if pool == nil {
		return
	}
	_, _ = pool.Exec(ctx,
		`INSERT INTO processed_emails_meta (key, checked_at) VALUES ('last_check', $1)
		 ON CONFLICT (key) DO UPDATE SET checked_at = $1`, t)
}

// buildCheckQuery constructs the Gmail search query for the no-arg check mode.
// since is the last successful check time; if zero, defaults to 1h ago.
// now is the current time (passed for testability).
func buildCheckQuery(since time.Time, now time.Time) (query string, effectiveSince time.Time) {
	effectiveSince = since
	if effectiveSince.IsZero() {
		effectiveSince = now.Add(-1 * time.Hour)
	}
	query = fmt.Sprintf("after:%d -in:trash -in:spam", effectiveSince.Unix())
	return
}

// checkNewEmails fetches emails since the last successful check, filters out
// already-seen IDs, records ALL new ones as processed, and triages each in a
// background goroutine (when triageCfg is non-nil). Returns only the first
// maxBatch emails to the orchestrator for immediate action.
//
// Searches all mail (inbox, sent, archived) except trash and spam.
func checkNewEmails(ctx context.Context, svc *gm.Service, pool *pgxpool.Pool, triageCfg *pipelines.TriageConfig, displayBatch int) (string, error) {
	since := lastEmailCheck(ctx, pool)
	checkStart := time.Now()
	query, _ := buildCheckQuery(since, checkStart)

	emails, err := gmail.FetchEmails(svc, query, 20)
	if err != nil {
		return fmt.Sprintf("Failed to fetch emails: %v", err), nil
	}

	// Persist the check timestamp regardless of results — prevents the window
	// from growing unbounded if there happen to be no emails for a while.
	saveEmailCheck(ctx, pool, checkStart)

	if len(emails) == 0 {
		return "No new emails since last check.", nil
	}

	tracker := &pipelines.ProcessedTracker{Pool: pool, Table: "processed_emails", IDColumn: "message_id"}
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
			pipelines.TriageAndSaveEmailAsync(*triageCfg, e)
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

// searchGmail performs a direct Gmail API search with query and optional time filters.
func searchGmail(svc *gm.Service, a searchEmailArgs) (string, error) {
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
