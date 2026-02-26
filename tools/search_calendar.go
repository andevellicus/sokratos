package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type searchCalendarArgs struct {
	Query      string `json:"query"`
	Title      string `json:"title"`
	Attendee   string `json:"attendee"`
	StartDate  string `json:"start_date,omitempty"` // ISO8601
	EndDate    string `json:"end_date,omitempty"`   // ISO8601
	MaxResults int64  `json:"max_results"`
}

// NewSearchCalendar returns a ToolFunc that searches locally embedded calendar
// events via pgvector, weighted by salience. Supports optional title/attendee
// filters that regex-match against the event summary lines.
func NewSearchCalendar(pool *pgxpool.Pool, embedEndpoint, embedModel string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchCalendarArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Query == "" && a.Title == "" && a.Attendee == "" {
			return "error: at least one of query, title, or attendee is required", nil
		}
		if a.MaxResults <= 0 {
			a.MaxResults = 10
		}

		var filters []RegexFilter
		var fallback []string
		if a.Title != "" {
			filters = append(filters, RegexFilter{FieldPrefix: "Title:", Value: a.Title})
			fallback = append(fallback, a.Title)
		}
		if a.Attendee != "" {
			filters = append(filters, RegexFilter{FieldPrefix: "Attendees:", Value: a.Attendee})
			fallback = append(fallback, a.Attendee)
		}

		var startDate, endDate *time.Time
		if a.StartDate != "" {
			t, err := parseISO8601(a.StartDate)
			if err != nil {
				return fmt.Sprintf("invalid start_date %q: %v", a.StartDate, err), nil
			}
			startDate = &t
		}
		if a.EndDate != "" {
			t, err := parseISO8601(a.EndDate)
			if err != nil {
				return fmt.Sprintf("invalid end_date %q: %v", a.EndDate, err), nil
			}
			endDate = &t
		}

		return SearchMemoriesByDomain(ctx, pool, embedEndpoint, embedModel, MemorySearchConfig{
			DomainTag:     "calendar",
			Filters:       filters,
			QueryText:     a.Query,
			FallbackParts: fallback,
			MaxResults:    a.MaxResults,
			StartDate:     startDate,
			EndDate:       endDate,
			ResultNoun:    "calendar event",
			ResultLabel:   "Event",
			ErrorPrefix:   "Calendar search",
		})
	}
}
