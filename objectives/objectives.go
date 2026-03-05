package objectives

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Objective represents a row from the objectives table.
type Objective struct {
	ID            int64
	Summary       string
	Status        string // active, in_progress, completed, paused, retired
	Priority      string // high, medium, low
	Source        string // explicit, inferred
	ProgressNotes string
	Attempts      int
	LastPursued   *time.Time
	CreatedAt     time.Time
}

// Create inserts a new objective and returns its ID.
func Create(ctx context.Context, db *pgxpool.Pool, summary, priority, source string) (int64, error) {
	if priority == "" {
		priority = "medium"
	}
	if source == "" {
		source = "explicit"
	}
	var id int64
	err := db.QueryRow(ctx,
		`INSERT INTO objectives (summary, priority, source) VALUES ($1, $2, $3) RETURNING id`,
		summary, priority, source,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create objective: %w", err)
	}
	return id, nil
}

// UpdateStatus sets an objective's status and updates updated_at.
func UpdateStatus(ctx context.Context, db *pgxpool.Pool, id int64, status string) error {
	_, err := db.Exec(ctx,
		`UPDATE objectives SET status = $1, updated_at = now() WHERE id = $2`,
		status, id)
	return err
}

// AppendProgress appends a note to the objective's progress_notes.
func AppendProgress(ctx context.Context, db *pgxpool.Pool, id int64, note string) error {
	_, err := db.Exec(ctx,
		`UPDATE objectives SET progress_notes = CASE
			WHEN progress_notes IS NULL OR progress_notes = '' THEN $1
			ELSE progress_notes || E'\n---\n' || $1
		 END, updated_at = now() WHERE id = $2`,
		note, id)
	return err
}

// IncrementAttempts bumps attempts and sets last_pursued_at.
func IncrementAttempts(ctx context.Context, db *pgxpool.Pool, id int64) error {
	_, err := db.Exec(ctx,
		`UPDATE objectives SET attempts = attempts + 1, last_pursued_at = now(), updated_at = now() WHERE id = $1`,
		id)
	return err
}

// Get retrieves a single objective by ID.
func Get(ctx context.Context, db *pgxpool.Pool, id int64) (*Objective, error) {
	g := &Objective{}
	var notes *string
	err := db.QueryRow(ctx,
		`SELECT id, summary, status, priority, source, progress_notes, attempts, last_pursued_at, created_at
		 FROM objectives WHERE id = $1`, id,
	).Scan(&g.ID, &g.Summary, &g.Status, &g.Priority, &g.Source, &notes, &g.Attempts, &g.LastPursued, &g.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get objective: %w", err)
	}
	if notes != nil {
		g.ProgressNotes = *notes
	}
	return g, nil
}

// ListActive returns objectives with status IN ('active', 'in_progress'), ordered
// by priority rank (high=1, medium=2, low=3) then updated_at DESC.
func ListActive(ctx context.Context, db *pgxpool.Pool) ([]Objective, error) {
	rows, err := db.Query(ctx,
		`SELECT id, summary, status, priority, source, progress_notes, attempts, last_pursued_at, created_at
		 FROM objectives
		 WHERE status IN ('active', 'in_progress')
		 ORDER BY CASE priority WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
		          updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list active objectives: %w", err)
	}
	defer rows.Close()
	return scanObjectives(rows)
}

// ListAll returns all non-retired objectives, limited to 20.
func ListAll(ctx context.Context, db *pgxpool.Pool) ([]Objective, error) {
	rows, err := db.Query(ctx,
		`SELECT id, summary, status, priority, source, progress_notes, attempts, last_pursued_at, created_at
		 FROM objectives
		 WHERE status != 'retired'
		 ORDER BY CASE priority WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
		          updated_at DESC
		 LIMIT 20`)
	if err != nil {
		return nil, fmt.Errorf("list all objectives: %w", err)
	}
	defer rows.Close()
	return scanObjectives(rows)
}

// Retire sets an objective's status to 'retired'.
func Retire(ctx context.Context, db *pgxpool.Pool, id int64) error {
	return UpdateStatus(ctx, db, id, "retired")
}

// Complete marks an objective as completed with a timestamp.
func Complete(ctx context.Context, db *pgxpool.Pool, id int64) error {
	_, err := db.Exec(ctx,
		`UPDATE objectives SET status = 'completed', completed_at = now(), updated_at = now() WHERE id = $1`,
		id)
	return err
}

// FindSimilar returns objectives whose summary matches the query via ILIKE.
// Used for dedup before creating new objectives.
func FindSimilar(ctx context.Context, db *pgxpool.Pool, summary string) ([]Objective, error) {
	// Use first 60 chars as a fuzzy match key.
	key := summary
	if len(key) > 60 {
		key = key[:60]
	}
	rows, err := db.Query(ctx,
		`SELECT id, summary, status, priority, source, progress_notes, attempts, last_pursued_at, created_at
		 FROM objectives
		 WHERE status NOT IN ('retired', 'completed')
		   AND summary ILIKE '%' || $1 || '%'
		 LIMIT 5`, key)
	if err != nil {
		return nil, fmt.Errorf("find similar objectives: %w", err)
	}
	defer rows.Close()
	return scanObjectives(rows)
}

// UpdatePriority changes an objective's priority.
func UpdatePriority(ctx context.Context, db *pgxpool.Pool, id int64, priority string) error {
	_, err := db.Exec(ctx,
		`UPDATE objectives SET priority = $1, updated_at = now() WHERE id = $2`,
		priority, id)
	return err
}

// FormatList formats a slice of objectives into a human-readable string.
func FormatList(objectives []Objective) string {
	if len(objectives) == 0 {
		return "No objectives found."
	}
	var b strings.Builder
	for _, g := range objectives {
		fmt.Fprintf(&b, "#%d [%s] (%s, %s) %s", g.ID, g.Status, g.Priority, g.Source, g.Summary)
		if g.Attempts > 0 {
			fmt.Fprintf(&b, " | attempts: %d", g.Attempts)
		}
		if g.LastPursued != nil {
			fmt.Fprintf(&b, " | last pursued: %s", g.LastPursued.Format(time.RFC3339))
		}
		b.WriteString("\n")
		if g.ProgressNotes != "" {
			fmt.Fprintf(&b, "  Progress: %s\n", truncate(g.ProgressNotes, 200))
		}
	}
	return b.String()
}

func scanObjectives(rows interface {
	Next() bool
	Scan(...any) error
}) ([]Objective, error) {
	var result []Objective
	for rows.Next() {
		var g Objective
		var notes *string
		if err := rows.Scan(&g.ID, &g.Summary, &g.Status, &g.Priority, &g.Source,
			&notes, &g.Attempts, &g.LastPursued, &g.CreatedAt); err != nil {
			continue
		}
		if notes != nil {
			g.ProgressNotes = *notes
		}
		result = append(result, g)
	}
	return result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
