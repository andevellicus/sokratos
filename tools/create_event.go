package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"sokratos/google"
	"sokratos/logger"
	"sokratos/timefmt"

	cal "google.golang.org/api/calendar/v3"
)

type createEventArgs struct {
	Title       string   `json:"title"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Description string   `json:"description"`
	Location    string   `json:"location"`
	Attendees   []string `json:"attendees"`
}

// NewCreateEvent returns a ToolFunc that creates a new Google Calendar event.
func NewCreateEvent(svc *cal.Service) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a createEventArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Title == "" {
			return "error: 'title' is required", nil
		}
		if a.Start == "" {
			return "error: 'start' is required (RFC3339 format)", nil
		}

		startTime, err := timefmt.ParseISO8601(a.Start)
		if err != nil {
			return fmt.Sprintf("error: invalid start time: %v (expected ISO8601/RFC3339 format)", err), nil
		}
		// LLMs commonly append "Z" (UTC) to timestamps when they mean local
		// time. Reinterpret UTC wall-clock values as local to prevent events
		// from being created hours off from the user's intent.
		startTime = timefmt.ReinterpretAsLocal(startTime)

		endTime := startTime.Add(1 * time.Hour) // default: 1 hour
		if a.End != "" {
			if t, err := timefmt.ParseISO8601(a.End); err == nil {
				endTime = timefmt.ReinterpretAsLocal(t)
			} else {
				return fmt.Sprintf("error: invalid end time: %v (expected ISO8601/RFC3339 format)", err), nil
			}
		}

		event, err := google.CreateEvent(svc, a.Title, a.Description, a.Location, startTime, endTime, a.Attendees)
		if err != nil {
			logger.Log.Errorf("[create_event] failed: %v", err)
			return fmt.Sprintf("Failed to create event: %v", err), nil
		}

		logger.Log.Infof("[create_event] created %q at %s", event.Summary, event.Start.Format(time.RFC3339))

		result := fmt.Sprintf("Event created successfully:\nTitle: %s\nStart: %s\nEnd: %s",
			event.Summary,
			timefmt.FormatDateTime(event.Start),
			timefmt.FormatDateTime(event.End))
		if event.Location != "" {
			result += fmt.Sprintf("\nLocation: %s", event.Location)
		}
		if event.HtmlLink != "" {
			result += fmt.Sprintf("\nLink: %s", event.HtmlLink)
		}

		return result, nil
	}
}
