package main

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/google"
	"sokratos/tools"
)

// registerCalendarTools registers tools for searching and creating calendar events.
func registerCalendarTools(registry *tools.Registry, pool *pgxpool.Pool) {
	if google.CalendarService == nil {
		return
	}
	registry.Register("search_calendar", tools.NewSearchCalendar(google.CalendarService, pool), tools.ToolSchema{
		Name:          "search_calendar",
		Description:   "Search Google Calendar for events with optional time bounds",
		ProgressLabel: "Checking calendar...",
		Params: []tools.ParamSchema{
			{Name: "query", Type: "string", Required: false},
			{Name: "time_min", Type: "string", Required: false},
			{Name: "time_max", Type: "string", Required: false},
			{Name: "max_results", Type: "number", Required: false},
		},
	})
	registry.Register("create_event", tools.NewCreateEvent(google.CalendarService), tools.ToolSchema{
		Name:          "create_event",
		Description:   "Create a Google Calendar event. Use the user's local timezone offset in start/end times (e.g. 2026-03-07T19:00:00-05:00), NOT Z/UTC.",
		ProgressLabel: "Creating event...",
		Params: []tools.ParamSchema{
			{Name: "title", Type: "string", Required: true},
			{Name: "start", Type: "string", Required: true},
			{Name: "end", Type: "string", Required: false},
			{Name: "description", Type: "string", Required: false},
			{Name: "location", Type: "string", Required: false},
			{Name: "attendees", Type: "array", Required: false},
		},
		ConfirmFormat: func(args json.RawMessage) string {
			var a struct {
				Title string `json:"title"`
				Start string `json:"start"`
			}
			_ = json.Unmarshal(args, &a)
			return fmt.Sprintf("⚠️ Create calendar event\n%q at %s", a.Title, a.Start)
		},
		ConfirmCacheKey: func(args json.RawMessage) string {
			var a struct {
				Title string `json:"title"`
				Start string `json:"start"`
			}
			if json.Unmarshal(args, &a) == nil {
				return "create_event:" + a.Title + ":" + a.Start
			}
			return "create_event"
		},
	})
}
