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
	"sokratos/memory"
	"sokratos/prompts"
)

type checkCalendarArgs struct {
	Days int `json:"days"`
}

// NewCheckCalendar returns a ToolFunc that fetches upcoming calendar events,
// skips already-processed ones, triages new ones through the Deep Thinker,
// and saves all of them to memory with full-text embedding.
func NewCheckCalendar(svc *cal.Service, pool *pgxpool.Pool, embedEndpoint, embedModel string, dtc *DeepThinkerClient, granite memory.GraniteFunc) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		days := 7
		if args != nil && len(args) > 2 {
			var a checkCalendarArgs
			if err := json.Unmarshal(args, &a); err == nil && a.Days > 0 {
				days = a.Days
			}
		}

		timeMin := time.Now()
		timeMax := timeMin.AddDate(0, 0, days)

		events, err := calendar.FetchUpcoming(svc, 50, timeMin)
		if err != nil {
			return fmt.Sprintf("Failed to fetch calendar events: %v", err), nil
		}

		// Filter to events within the requested window.
		var filtered []calendar.Event
		for _, e := range events {
			if e.Start.Before(timeMax) {
				filtered = append(filtered, e)
			}
		}
		events = filtered

		if len(events) == 0 {
			return fmt.Sprintf("No upcoming events in the next %d days.", days), nil
		}

		// Filter out already-processed events.
		events, err = FilterProcessed(ctx, pool, ProcessedEvents, events, func(e calendar.Event) string { return e.ID })
		if err != nil {
			logger.Log.Warnf("[check_calendar] dedup check failed, processing all: %v", err)
		}

		if len(events) == 0 {
			return "No new calendar events since last check.", nil
		}

		items := make([]CheckItem, len(events))
		for i, e := range events {
			e := e // capture for closure
			var srcDate *time.Time
			if !e.Start.IsZero() {
				d := e.Start
				srcDate = &d
			}
			items[i] = CheckItem{
				ID:           e.ID,
				DisplayLabel: fmt.Sprintf("%q", e.Summary),
				TriageText:   calendar.FormatEventSummary(e),
				EmbeddingText: func(triageLine string) string {
					return calendar.FormatForEmbedding(e, triageLine)
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
			LogPrefix:      "[check_calendar]",
			DomainTag:      "calendar",
			TriagePrompt:   strings.TrimSpace(prompts.CalendarTriage),
			ProcessedTable: ProcessedEvents,
		}, items), nil
	}
}

