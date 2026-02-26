package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type searchInboxArgs struct {
	Query      string `json:"query"`
	From       string `json:"from"`
	To         string `json:"to"`
	Subject    string `json:"subject"`
	StartDate  string `json:"start_date,omitempty"` // ISO8601
	EndDate    string `json:"end_date,omitempty"`   // ISO8601
	MaxResults int64  `json:"max_results"`
}

// NewSearchInbox returns a ToolFunc that searches locally embedded emails
// via pgvector, weighted by salience. Supports optional from/to/subject
// filters that regex-match against the email header lines.
func NewSearchInbox(pool *pgxpool.Pool, embedEndpoint, embedModel string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchInboxArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Query == "" && a.From == "" && a.To == "" && a.Subject == "" {
			return "error: at least one of query, from, to, or subject is required", nil
		}
		if a.MaxResults <= 0 {
			a.MaxResults = 10
		}

		var filters []RegexFilter
		var fallback []string
		if a.From != "" {
			filters = append(filters, RegexFilter{FieldPrefix: "From:", Value: a.From})
			fallback = append(fallback, a.From)
		}
		if a.To != "" {
			filters = append(filters, RegexFilter{FieldPrefix: "To:", Value: a.To})
			fallback = append(fallback, a.To)
		}
		if a.Subject != "" {
			filters = append(filters, RegexFilter{FieldPrefix: "Subject:", Value: a.Subject})
			fallback = append(fallback, a.Subject)
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
			DomainTag:     "email",
			Filters:       filters,
			QueryText:     a.Query,
			FallbackParts: fallback,
			MaxResults:    a.MaxResults,
			StartDate:     startDate,
			EndDate:       endDate,
			ResultNoun:    "email",
			ResultLabel:   "Email",
			ErrorPrefix:   "Email search",
		})
	}
}
