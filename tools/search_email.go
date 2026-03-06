package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	gm "google.golang.org/api/gmail/v1"

	"sokratos/clients"
	"sokratos/google"
	"sokratos/logger"
	"sokratos/pipelines"
	"sokratos/textutil"
	"sokratos/timefmt"
	"sokratos/tokens"
)

type searchEmailArgs struct {
	Query      string  `json:"query"`
	MaxResults float64 `json:"max_results"`
	TimeMin    string  `json:"time_min"`
	TimeMax    string  `json:"time_max"`
}

// emailBodyCap is the per-email body cap when subagent summarization is unavailable.
const emailBodyCap = 2000

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
//
// When sc is non-nil, email bodies in the display batch are summarized via
// subagent in parallel (non-blocking — falls back to truncated body if busy).
func NewSearchEmail(svc *gm.Service, pool *pgxpool.Pool, triageCfg *pipelines.TriageConfig, displayBatch int, sc *clients.SubagentClient) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchEmailArgs
		if len(args) > 2 {
			if err := json.Unmarshal(args, &a); err != nil {
				return "", Errorf("invalid arguments: %v", err)
			}
		}

		// No-arg mode: dedup check for new emails.
		if a.Query == "" && a.TimeMin == "" && a.TimeMax == "" {
			return checkNewEmails(ctx, svc, pool, triageCfg, displayBatch, sc)
		}

		// Search mode: pass-through to Gmail.
		return searchGmail(ctx, svc, a, sc)
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
func checkNewEmails(ctx context.Context, svc *gm.Service, pool *pgxpool.Pool, triageCfg *pipelines.TriageConfig, displayBatch int, sc *clients.SubagentClient) (string, error) {
	since := lastEmailCheck(ctx, pool)
	checkStart := time.Now()
	query, _ := buildCheckQuery(since, checkStart)

	emails, err := google.FetchEmails(svc, query, 20)
	if err != nil {
		if google.IsAuthError(err) {
			return "", Errorf("%s", google.AuthErrorMessage)
		}
		return "", Errorf("Failed to fetch emails: %v", err)
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
	var newEmails []google.Email
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
	return formatEmails(ctx, batch, header, sc), nil
}

// searchGmail performs a direct Gmail API search with query and optional time filters.
func searchGmail(ctx context.Context, svc *gm.Service, a searchEmailArgs, sc *clients.SubagentClient) (string, error) {
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

	emails, err := google.FetchEmails(svc, finalQuery, maxResults)
	if err != nil {
		if google.IsAuthError(err) {
			return "", Errorf("%s", google.AuthErrorMessage)
		}
		logger.Log.Errorf("[search_email] failed: %v", err)
		return "", Errorf("Failed to search email: %v", err)
	}
	if len(emails) == 0 {
		return "No emails found matching your query.", nil
	}

	logger.Log.Infof("[search_email] %d results found for %q", len(emails), finalQuery)
	return formatEmails(ctx, emails, fmt.Sprintf("Found %d email(s):", len(emails)), sc), nil
}

// formatEmails formats a batch of emails for the orchestrator. When sc is
// non-nil, email bodies are summarized in parallel via subagent; otherwise
// the raw body is truncated.
func formatEmails(ctx context.Context, emails []google.Email, header string, sc *clients.SubagentClient) string {
	summaries := summarizeEmailBodies(ctx, sc, emails)

	var b strings.Builder
	b.WriteString(header)
	for i, e := range emails {
		fmt.Fprintf(&b, "\n\n--- Email %d ---\n", i+1)
		fmt.Fprintf(&b, "From: %s\nTo: %s\n", e.From, e.To)
		if e.CC != "" {
			fmt.Fprintf(&b, "CC: %s\n", e.CC)
		}
		if e.BCC != "" {
			fmt.Fprintf(&b, "BCC: %s\n", e.BCC)
		}
		dateStr := ""
		if !e.Date.IsZero() {
			dateStr = timefmt.FormatDateTime(e.Date)
		}
		fmt.Fprintf(&b, "Subject: %s\nDate: %s\n\n", e.Subject, dateStr)

		if summary, ok := summaries[i]; ok {
			b.WriteString(summary)
		} else {
			body := e.Body
			if body == "" {
				body = e.Snippet
			}
			b.WriteString(textutil.Truncate(body, emailBodyCap))
		}
	}
	return b.String()
}

// summarizeEmailBodies fans out subagent summarization for email bodies in
// parallel. Uses TryComplete (non-blocking) — returns an empty map if no
// subagent is available or all slots are busy.
func summarizeEmailBodies(ctx context.Context, sc *clients.SubagentClient, emails []google.Email) map[int]string {
	if sc == nil {
		return nil
	}

	type result struct {
		index   int
		summary string
	}

	ch := make(chan result, len(emails))
	var wg sync.WaitGroup

	systemPrompt := "Summarize this email concisely. Extract the key information: what is being communicated, any action items or deadlines, and important details. Strip signatures, disclaimers, quoted reply chains, and boilerplate. Return only the summary, no preamble."

	for i, e := range emails {
		if e.Body == "" {
			continue
		}
		wg.Add(1)
		go func(idx int, e google.Email) {
			defer wg.Done()
			userContent := fmt.Sprintf("From: %s\nSubject: %s\n\n%s", e.From, e.Subject, textutil.Truncate(e.Body, 4000))
			summary, err := sc.TryComplete(ctx, systemPrompt, userContent, tokens.EmailSummary)
			if err != nil {
				return
			}
			ch <- result{index: idx, summary: strings.TrimSpace(summary)}
		}(i, e)
	}
	go func() { wg.Wait(); close(ch) }()

	summaries := make(map[int]string)
	for r := range ch {
		if r.summary != "" {
			summaries[r.index] = r.summary
		}
	}

	if len(summaries) > 0 {
		logger.Log.Infof("[search_email] summarized %d/%d email bodies via subagent", len(summaries), len(emails))
	}
	return summaries
}
