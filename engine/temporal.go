package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/timefmt"
	"sokratos/timeouts"
)

// BuildTemporalContext queries the DB for recent high-salience memories and
// upcoming tasks, formatting them as an XML block for system prompt injection.
// Returns empty string if db is nil or all queries fail.
func BuildTemporalContext(ctx context.Context, db *pgxpool.Pool) string {
	if db == nil {
		return ""
	}

	qCtx, cancel := context.WithTimeout(ctx, timeouts.DBQuery)
	defer cancel()

	var b strings.Builder
	b.WriteString("<temporal_context>\n")
	fmt.Fprintf(&b, "  <now>%s</now>\n", timefmt.Now())

	// Recent high-salience memories (last 7 days).
	rows, err := db.Query(qCtx,
		`SELECT summary, created_at FROM memories
		 WHERE created_at >= NOW() - INTERVAL '7 days'
		   AND salience >= 6
		   AND COALESCE(source, '') != 'backfill'
		 ORDER BY created_at DESC LIMIT 8`)
	if err == nil {
		var items []string
		for rows.Next() {
			var summary string
			var ts time.Time
			if rows.Scan(&summary, &ts) == nil {
				items = append(items, fmt.Sprintf("    <event time=\"%s\" ago=\"%s\">%s</event>",
					ts.Format(timefmt.DateTime), relativeTime(ts), summary))
			}
		}
		rows.Close()
		if len(items) > 0 {
			b.WriteString("  <recent_timeline>\n")
			for _, item := range items {
				b.WriteString(item)
				b.WriteString("\n")
			}
			b.WriteString("  </recent_timeline>\n")
		}
	}

	// Upcoming scheduled tasks (next 48 hours).
	taskRows, err := db.Query(qCtx,
		`SELECT directive, due_at FROM work_items
		 WHERE type = 'scheduled' AND status = 'pending' AND due_at IS NOT NULL
		   AND due_at BETWEEN NOW() AND NOW() + INTERVAL '48 hours'
		 ORDER BY due_at ASC LIMIT 5`)
	if err == nil {
		var items []string
		for taskRows.Next() {
			var desc string
			var due time.Time
			if taskRows.Scan(&desc, &due) == nil {
				items = append(items, fmt.Sprintf("    <upcoming time=\"%s\" in=\"%s\">%s</upcoming>",
					due.Format(timefmt.DateTime), relativeTime(due), desc))
			}
		}
		taskRows.Close()
		if len(items) > 0 {
			b.WriteString("  <upcoming_timeline>\n")
			for _, item := range items {
				b.WriteString(item)
				b.WriteString("\n")
			}
			b.WriteString("  </upcoming_timeline>\n")
		}
	}

	b.WriteString("</temporal_context>")
	return b.String()
}

// relativeTime returns a human-readable relative time string.
// Handles both past ("2h ago") and future ("in 3h") times.
func relativeTime(t time.Time) string {
	d := time.Since(t)
	future := d < 0
	if future {
		d = -d
	}

	var label string
	switch {
	case d < time.Minute:
		label = "just now"
		if future {
			label = "now"
		}
		return label
	case d < time.Hour:
		label = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		label = fmt.Sprintf("%dh", int(d.Hours()))
	default:
		label = fmt.Sprintf("%dd", int(d.Hours()/24))
	}

	if future {
		return "in " + label
	}
	return label + " ago"
}
