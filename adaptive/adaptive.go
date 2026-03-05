package adaptive

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/timeouts"
)

// Param represents a single adaptive parameter with its metadata.
type Param struct {
	Key       string
	Value     float64
	Source    string
	Reason    string
	UpdatedAt time.Time
}

// Get returns the current value of an adaptive parameter, falling back to
// defaultVal on any error (fail-open).
func Get(ctx context.Context, db *pgxpool.Pool, key string, defaultVal float64) float64 {
	if db == nil {
		return defaultVal
	}
	var val float64
	err := db.QueryRow(ctx,
		`SELECT value FROM adaptive_params WHERE key = $1`, key,
	).Scan(&val)
	if err != nil {
		return defaultVal
	}
	return val
}

// Set upserts an adaptive parameter with the given source and reason.
func Set(ctx context.Context, db *pgxpool.Pool, key string, value float64, source, reason string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(ctx,
		`INSERT INTO adaptive_params (key, value, source, reason, updated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (key) DO UPDATE SET value = $2, source = $3, reason = $4, updated_at = now()`,
		key, value, source, reason,
	)
	if err != nil {
		logger.Log.Warnf("[adaptive] failed to set %s=%.2f: %v", key, value, err)
	}
	return err
}

// GetAll returns all adaptive parameters. Returns nil on error.
func GetAll(ctx context.Context, db *pgxpool.Pool) []Param {
	if db == nil {
		return nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeouts.DBQuery)
	defer cancel()

	rows, err := db.Query(queryCtx,
		`SELECT key, value, source, COALESCE(reason, ''), updated_at FROM adaptive_params ORDER BY key`,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var params []Param
	for rows.Next() {
		var p Param
		if err := rows.Scan(&p.Key, &p.Value, &p.Source, &p.Reason, &p.UpdatedAt); err != nil {
			continue
		}
		params = append(params, p)
	}
	return params
}

// paramRanges defines the clamped ranges for known adaptive parameters.
var paramRanges = map[string][2]float64{
	"triage_conversation_threshold":            {1, 8},
	"triage_conversation_unverified_threshold": {3, 8},
	"triage_email_threshold":                   {1, 5},
	"curiosity_cooldown_hours":                 {0.5, 6},
}

// Clamp returns value clamped to the known range for the given key.
// Returns value unchanged if key is unknown.
func Clamp(key string, value float64) float64 {
	r, ok := paramRanges[key]
	if !ok {
		return value
	}
	if value < r[0] {
		return r[0]
	}
	if value > r[1] {
		return r[1]
	}
	return value
}

// IsValidKey returns true if the key is a known adaptive parameter.
func IsValidKey(key string) bool {
	_, ok := paramRanges[key]
	return ok
}
