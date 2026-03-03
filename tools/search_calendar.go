package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	cal "google.golang.org/api/calendar/v3"

	"sokratos/calendar"
	"sokratos/logger"
	"sokratos/pipelines"
	"sokratos/timefmt"
)

type searchCalendarArgs struct {
	Query      string  `json:"query"`
	TimeMin    string  `json:"time_min,omitempty"` // ISO8601
	TimeMax    string  `json:"time_max,omitempty"` // ISO8601
	MaxResults float64 `json:"max_results"`
}

// temporalOnlyQuery returns true when the query consists entirely of temporal
// and date words (e.g. "today", "February 27 2026", "today February 27 2026")
// that should not be passed to the Google Calendar API's Q parameter, which
// does full-text search on event content. When time bounds already express the
// temporal intent, passing these words as text search would filter out events
// that don't literally contain the words.
func temporalOnlyQuery(q string) bool {
	temporal := map[string]bool{
		"today": true, "tomorrow": true, "yesterday": true,
		"tonight": true, "now": true, "upcoming": true, "soon": true,
		"this": true, "next": true, "last": true,
		"week": true, "weekend": true, "month": true,
		"morning": true, "evening": true, "afternoon": true,
		// Month names
		"january": true, "february": true, "march": true, "april": true,
		"may": true, "june": true, "july": true, "august": true,
		"september": true, "october": true, "november": true, "december": true,
		// Day names
		"monday": true, "tuesday": true, "wednesday": true, "thursday": true,
		"friday": true, "saturday": true, "sunday": true,
	}

	words := strings.Fields(strings.ToLower(strings.TrimSpace(q)))
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		// Strip trailing punctuation first (e.g. commas in "February 27, 2026")
		w = strings.TrimRight(w, ".,;:")
		// Accept pure numbers (day/year: "27", "2026")
		allDigits := true
		for _, c := range w {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			continue
		}
		if !temporal[w] {
			return false
		}
	}
	return true
}

// eventUID returns a stable identifier for dedup. Prefers ICalUID (RFC5545),
// falls back to the Google event ID.
func eventUID(e calendar.Event) string {
	if e.ICalUID != "" {
		return e.ICalUID
	}
	return e.ID
}

// filterNewEvents removes events whose UID is already in processed_events.
func filterNewEvents(ctx context.Context, pool *pgxpool.Pool, events []calendar.Event) []calendar.Event {
	tracker := &pipelines.ProcessedTracker{Pool: pool, Table: "processed_events", IDColumn: "event_uid"}
	uids := make([]string, len(events))
	for i, e := range events {
		uids[i] = eventUID(e)
	}
	newUIDs := tracker.FilterNew(ctx, uids)
	newUIDSet := make(map[string]bool, len(newUIDs))
	for _, uid := range newUIDs {
		newUIDSet[uid] = true
	}
	var newEvents []calendar.Event
	for _, e := range events {
		if newUIDSet[eventUID(e)] {
			newEvents = append(newEvents, e)
		}
	}
	return newEvents
}

// markEventsProcessed inserts event UIDs into processed_events.
func markEventsProcessed(ctx context.Context, pool *pgxpool.Pool, events []calendar.Event) {
	tracker := &pipelines.ProcessedTracker{Pool: pool, Table: "processed_events", IDColumn: "event_uid"}
	uids := make([]string, len(events))
	for i, e := range events {
		uids[i] = eventUID(e)
	}
	tracker.MarkProcessed(ctx, uids)
}

// NewSearchCalendar returns a ToolFunc that searches Google Calendar directly.
// When pool is non-nil and the call has no arguments (routine mode), events are
// deduplicated against processed_events so the orchestrator only sees new ones.
func NewSearchCalendar(svc *cal.Service, pool *pgxpool.Pool) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchCalendarArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}

		// Detect routine mode: no explicit arguments provided.
		routineMode := pool != nil && a.Query == "" && a.TimeMin == "" && a.TimeMax == ""

		maxResults := int64(25)
		if a.MaxResults > 0 {
			maxResults = int64(a.MaxResults)
		}

		var timeMin, timeMax *time.Time
		if a.TimeMin != "" {
			t, err := parseISO8601(a.TimeMin)
			if err != nil {
				return fmt.Sprintf("invalid time_min %q: %v", a.TimeMin, err), nil
			}
			t = timefmt.ReinterpretAsLocal(t)
			timeMin = &t
		} else {
			// Default to now — calendar searches without explicit time bounds
			// should show current/future events, not pull in past events.
			t := time.Now()
			timeMin = &t
		}

		if a.TimeMax != "" {
			t, err := parseISO8601(a.TimeMax)
			if err != nil {
				return fmt.Sprintf("invalid time_max %q: %v", a.TimeMax, err), nil
			}
			t = timefmt.ReinterpretAsLocal(t)
			timeMax = &t
		}

		// When time bounds already express the intent, strip purely temporal
		// queries (e.g. "today") so they don't filter by event text content.
		query := a.Query
		if (a.TimeMin != "" || a.TimeMax != "") && temporalOnlyQuery(query) {
			logger.Log.Infof("[search_calendar] stripped temporal-only query %q (time bounds already set)", query)
			query = ""
		}

		events, err := calendar.SearchEvents(svc, query, timeMin, timeMax, maxResults)
		if err != nil {
			logger.Log.Errorf("[search_calendar] failed: %v", err)
			return fmt.Sprintf("Failed to search calendar: %v", err), nil
		}

		// When Q + time bounds returns nothing, fall back to time-bounds-only
		// so the orchestrator sees what's actually on the calendar without
		// needing a second round-trip. The Q parameter does full-text search
		// and misses events where the person/topic isn't in the event title.
		droppedQuery := false
		if len(events) == 0 && query != "" && (timeMin != nil || timeMax != nil) {
			logger.Log.Infof("[search_calendar] Q=%q with time bounds returned 0 results, retrying without Q", query)
			events, err = calendar.SearchEvents(svc, "", timeMin, timeMax, maxResults)
			if err != nil {
				logger.Log.Errorf("[search_calendar] fallback failed: %v", err)
				return fmt.Sprintf("Failed to search calendar: %v", err), nil
			}
			droppedQuery = true
		}

		// In routine mode, filter out already-seen events.
		if routineMode && len(events) > 0 {
			events = filterNewEvents(ctx, pool, events)
			if len(events) == 0 {
				return "No new events since last check.", nil
			}
		}

		if len(events) == 0 {
			return "No events found matching your query.", nil
		}

		var b strings.Builder
		if droppedQuery {
			fmt.Fprintf(&b, "No events matched %q specifically, but found %d event(s) in the time range:\n\n", a.Query, len(events))
		} else {
			fmt.Fprintf(&b, "Found %d event(s):\n\n", len(events))
		}
		for i, e := range events {
			fmt.Fprintf(&b, "--- Event %d ---\n%s\n\n", i+1, calendar.FormatEventSummary(e))
		}

		// In routine mode, mark events as processed after formatting.
		if routineMode {
			markEventsProcessed(ctx, pool, events)
		}

		logger.Log.Infof("[search_calendar] %d results found for %q (fallback=%v, routine=%v)", len(events), a.Query, droppedQuery, routineMode)
		return b.String(), nil
	}
}
