package google

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	cal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"sokratos/logger"
	"sokratos/textutil"
	"sokratos/timefmt"
)

// CalendarService is the application-wide Google Calendar API client.
var CalendarService *cal.Service

// InitCalendarFromToken sets up the Google Calendar API client using only a previously
// saved token. No interactive flow — returns nil if no token exists. Used at
// startup for non-blocking initialization.
func InitCalendarFromToken(ctx context.Context, credentialsPath, tokenPath string) error {
	client, err := GetClientFromToken(ctx, "Calendar", credentialsPath, tokenPath, []string{cal.CalendarScope})
	if err != nil {
		return err
	}
	if client == nil {
		return nil
	}

	svc, err := cal.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("create calendar service: %w", err)
	}

	CalendarService = svc
	logger.Log.Info("Google Calendar API initialized (from token)")
	return nil
}

// InitCalendarFromClient sets up the Google Calendar API client from an already-authenticated
// HTTP client. Used when a single OAuth token covers both Gmail and Calendar.
func InitCalendarFromClient(ctx context.Context, client *http.Client) error {
	svc, err := cal.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("create calendar service: %w", err)
	}
	CalendarService = svc
	logger.Log.Info("Google Calendar API initialized")
	return nil
}

// Event holds parsed fields from a Google Calendar event.
type Event struct {
	ID           string
	Summary      string
	Description  string
	Location     string
	Start        time.Time
	End          time.Time
	AllDay       bool
	Organizer    string
	Attendees    []string // email addresses
	Status       string   // confirmed, tentative, cancelled
	HtmlLink     string
	CalendarID   string
	CalendarName string
	ICalUID      string // RFC5545 UID for cross-calendar dedup
}

// calInfo pairs a calendar ID with its display name.
type calInfo struct {
	ID   string
	Name string
}

// Calendar list cache with TTL.
var (
	calCache     []calInfo
	calCacheTime time.Time
	calCacheMu   sync.RWMutex
	calCacheTTL  = 10 * time.Minute
)

// cachedCalendars returns visible calendars, using a TTL cache to avoid
// hitting the CalendarList API on every search.
func cachedCalendars(svc *cal.Service) []calInfo {
	calCacheMu.RLock()
	if time.Since(calCacheTime) < calCacheTTL && len(calCache) > 0 {
		cached := calCache
		calCacheMu.RUnlock()
		return cached
	}
	calCacheMu.RUnlock()

	calCacheMu.Lock()
	defer calCacheMu.Unlock()
	// Double-check after write lock.
	if time.Since(calCacheTime) < calCacheTTL && len(calCache) > 0 {
		return calCache
	}
	cals, err := listVisibleCalendars(svc)
	if err != nil || len(cals) == 0 {
		logger.Log.Warnf("[calendar] listVisibleCalendars failed or empty (err=%v), falling back to primary", err)
		return []calInfo{{ID: "primary", Name: "Primary"}}
	}
	logger.Log.Debugf("[calendar] discovered %d calendars", len(cals))
	calCache = cals
	calCacheTime = time.Now()
	return cals
}

// InvalidateCache clears the cached calendar list so the next search
// re-discovers calendars. Called after /google re-auth.
func InvalidateCache() {
	calCacheMu.Lock()
	calCache = nil
	calCacheTime = time.Time{}
	calCacheMu.Unlock()
}

// ParseEvent extracts an Event from a raw Google Calendar API event.
func ParseEvent(e *cal.Event, calID, calName string) Event {
	ev := Event{
		ID:           e.Id,
		Summary:      e.Summary,
		Location:     e.Location,
		Status:       e.Status,
		HtmlLink:     e.HtmlLink,
		CalendarID:   calID,
		CalendarName: calName,
		ICalUID:      e.ICalUID,
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
			if t, err := time.Parse(timefmt.DateOnly, e.Start.Date); err == nil {
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
			if t, err := time.Parse(timefmt.DateOnly, e.End.Date); err == nil {
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
		events = append(events, ParseEvent(item, "primary", "Primary"))
	}

	return events, nil
}

// SearchEvents retrieves events matching a text query within a time range.
// It searches across all visible calendars (not just "primary") so that
// shared/family/subscribed calendars are included.
func SearchEvents(svc *cal.Service, query string, timeMin, timeMax *time.Time, maxResults int64) ([]Event, error) {
	if maxResults <= 0 {
		maxResults = 25
	}

	cals := cachedCalendars(svc)

	var allEvents []Event
	for _, c := range cals {
		req := svc.Events.List(c.ID).
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

		logger.Log.Debugf("[calendar] querying %s (timeMin=%v, timeMax=%v, q=%q)",
			c.Name, timeMin, timeMax, query)

		list, err := req.Do()
		if err != nil {
			return nil, fmt.Errorf("list events for %s: %w", c.Name, err)
		}

		for _, item := range list.Items {
			allEvents = append(allEvents, ParseEvent(item, c.ID, c.Name))
		}
	}

	// Deduplicate by ICalUID before sorting.
	allEvents = deduplicateEvents(allEvents)

	// Sort by start time and cap at maxResults.
	sort.Slice(allEvents, func(i, j int) bool {
		return allEvents[i].Start.Before(allEvents[j].Start)
	})
	if int64(len(allEvents)) > maxResults {
		allEvents = allEvents[:maxResults]
	}

	return allEvents, nil
}

// deduplicateEvents removes duplicate events by ICalUID (RFC5545), falling
// back to event ID when ICalUID is empty.
func deduplicateEvents(events []Event) []Event {
	seen := make(map[string]bool, len(events))
	result := make([]Event, 0, len(events))
	for _, e := range events {
		key := e.ICalUID
		if key == "" {
			key = e.ID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, e)
	}
	return result
}

// listVisibleCalendars returns info for all calendars the user can see.
func listVisibleCalendars(svc *cal.Service) ([]calInfo, error) {
	list, err := svc.CalendarList.List().Fields("items/id,items/summary").Do()
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	cals := make([]calInfo, len(list.Items))
	for i, item := range list.Items {
		name := item.Summary
		if name == "" {
			name = item.Id
		}
		cals[i] = calInfo{ID: item.Id, Name: name}
	}
	return cals, nil
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

	result := ParseEvent(created, "primary", "Primary")
	return &result, nil
}

// FormatEventSummary returns a human-readable summary of an event,
// with the description truncated at 500 characters.
func FormatEventSummary(e Event) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Title: %s\n", e.Summary)
	if e.CalendarName != "" {
		fmt.Fprintf(&b, "Calendar: %s\n", e.CalendarName)
	}
	if !e.Start.IsZero() {
		if e.AllDay {
			fmt.Fprintf(&b, "Date: %s (all day)\n", timefmt.FormatDate(e.Start))
		} else {
			fmt.Fprintf(&b, "Start: %s\n", timefmt.FormatDateTime(e.Start))
		}
	}
	if !e.End.IsZero() && !e.AllDay {
		fmt.Fprintf(&b, "End: %s\n", timefmt.FormatDateTime(e.End))
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

	desc := textutil.Truncate(e.Description, 500)
	if desc != "" {
		fmt.Fprintf(&b, "\n%s", desc)
	}

	return b.String()
}
