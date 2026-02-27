package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cal "google.golang.org/api/calendar/v3"

	"sokratos/calendar"
	"sokratos/logger"
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

// NewSearchCalendar returns a ToolFunc that searches Google Calendar directly.
func NewSearchCalendar(svc *cal.Service) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchCalendarArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}

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

		if len(events) == 0 {
			return "No events found matching your query.", nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "Found %d event(s):\n\n", len(events))
		for i, e := range events {
			fmt.Fprintf(&b, "--- Event %d ---\n%s\n\n", i+1, calendar.FormatEventSummary(e))
		}

		logger.Log.Infof("[search_calendar] %d results found for %q", len(events), a.Query)
		return b.String(), nil
	}
}
