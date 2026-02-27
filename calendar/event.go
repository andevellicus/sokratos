package calendar

import (
	"fmt"
	"strings"
	"time"

	cal "google.golang.org/api/calendar/v3"
)

// Event holds parsed fields from a Google Calendar event.
type Event struct {
	ID          string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	Organizer   string
	Attendees   []string // email addresses
	Status      string   // confirmed, tentative, cancelled
	HtmlLink    string
}

// ParseEvent extracts an Event from a raw Google Calendar API event.
func ParseEvent(e *cal.Event) Event {
	ev := Event{
		ID:       e.Id,
		Summary:  e.Summary,
		Location: e.Location,
		Status:   e.Status,
		HtmlLink: e.HtmlLink,
	}

	if e.Description != "" {
		ev.Description = e.Description
	}

	if e.Organizer != nil {
		ev.Organizer = e.Organizer.Email
	}

	for _, a := range e.Attendees {
		if a.Email != "" {
			ev.Attendees = append(ev.Attendees, a.Email)
		}
	}

	// Parse start time — all-day events use Date (YYYY-MM-DD), timed events use DateTime.
	if e.Start != nil {
		if e.Start.DateTime != "" {
			if t, err := time.Parse(time.RFC3339, e.Start.DateTime); err == nil {
				ev.Start = t
			}
		} else if e.Start.Date != "" {
			ev.AllDay = true
			if t, err := time.Parse("2006-01-02", e.Start.Date); err == nil {
				ev.Start = t
			}
		}
	}

	if e.End != nil {
		if e.End.DateTime != "" {
			if t, err := time.Parse(time.RFC3339, e.End.DateTime); err == nil {
				ev.End = t
			}
		} else if e.End.Date != "" {
			if t, err := time.Parse("2006-01-02", e.End.Date); err == nil {
				ev.End = t
			}
		}
	}

	return ev
}

// FetchUpcoming retrieves upcoming events from the primary calendar.
func FetchUpcoming(svc *cal.Service, maxResults int64, timeMin time.Time) ([]Event, error) {
	if maxResults <= 0 {
		maxResults = 25
	}

	list, err := svc.Events.List("primary").
		TimeMin(timeMin.Format(time.RFC3339)).
		SingleEvents(true).
		OrderBy("startTime").
		MaxResults(maxResults).
		Do()
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}

	var events []Event
	for _, item := range list.Items {
		events = append(events, ParseEvent(item))
	}

	return events, nil
}

// SearchEvents retrieves events matching a text query within a time range.
func SearchEvents(svc *cal.Service, query string, timeMin, timeMax *time.Time, maxResults int64) ([]Event, error) {
	if maxResults <= 0 {
		maxResults = 25
	}

	req := svc.Events.List("primary").
		SingleEvents(true).
		OrderBy("startTime").
		MaxResults(maxResults)

	if query != "" {
		req = req.Q(query)
	}
	if timeMin != nil {
		req = req.TimeMin(timeMin.Format(time.RFC3339))
	}
	if timeMax != nil {
		req = req.TimeMax(timeMax.Format(time.RFC3339))
	}

	list, err := req.Do()
	if err != nil {
		return nil, fmt.Errorf("search events: %w", err)
	}

	var events []Event
	for _, item := range list.Items {
		events = append(events, ParseEvent(item))
	}

	return events, nil
}

// CreateEvent creates a new event on the primary calendar.
func CreateEvent(svc *cal.Service, summary, description, location string, start, end time.Time, attendees []string) (*Event, error) {
	event := &cal.Event{
		Summary:     summary,
		Description: description,
		Location:    location,
		Start: &cal.EventDateTime{
			DateTime: start.Format(time.RFC3339),
		},
		End: &cal.EventDateTime{
			DateTime: end.Format(time.RFC3339),
		},
	}

	for _, email := range attendees {
		event.Attendees = append(event.Attendees, &cal.EventAttendee{Email: email})
	}

	created, err := svc.Events.Insert("primary", event).Do()
	if err != nil {
		return nil, fmt.Errorf("create event: %w", err)
	}

	result := ParseEvent(created)
	return &result, nil
}

// FormatEventSummary returns a human-readable summary of an event,
// with the description truncated at 500 characters.
func FormatEventSummary(e Event) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Title: %s\n", e.Summary)
	if !e.Start.IsZero() {
		if e.AllDay {
			fmt.Fprintf(&b, "Date: %s (all day)\n", e.Start.Format("2006-01-02"))
		} else {
			fmt.Fprintf(&b, "Start: %s\n", e.Start.Format("2006-01-02 15:04"))
		}
	}
	if !e.End.IsZero() && !e.AllDay {
		fmt.Fprintf(&b, "End: %s\n", e.End.Format("2006-01-02 15:04"))
	}
	if e.Location != "" {
		fmt.Fprintf(&b, "Location: %s\n", e.Location)
	}
	if e.Organizer != "" {
		fmt.Fprintf(&b, "Organizer: %s\n", e.Organizer)
	}
	if len(e.Attendees) > 0 {
		fmt.Fprintf(&b, "Attendees: %s\n", strings.Join(e.Attendees, ", "))
	}
	if e.Status != "" {
		fmt.Fprintf(&b, "Status: %s\n", e.Status)
	}

	desc := e.Description
	if len(desc) > 500 {
		desc = desc[:500] + "..."
	}
	if desc != "" {
		fmt.Fprintf(&b, "\n%s", desc)
	}

	return b.String()
}
