package routines

import (
	"fmt"
	"strings"
	"time"

	"sokratos/timefmt"
)

// NormalizeSchedule converts interface{} (string or []string from TOML/JSON)
// to a comma-separated DB string. Returns empty string for nil/unsupported types.
func NormalizeSchedule(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case []string:
		return strings.Join(s, ",")
	case []interface{}:
		parts := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				parts = append(parts, str)
			}
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}

// ParseSchedules splits a comma-separated schedule string into individual
// "HH:MM" entries.
func ParseSchedules(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// ValidateSchedules checks that every entry in the schedule value (string or
// []string) is a valid "HH:MM" format. Returns an error describing the first
// invalid entry found.
func ValidateSchedules(v interface{}) error {
	schedStr := NormalizeSchedule(v)
	if schedStr == "" {
		return nil
	}
	for _, s := range ParseSchedules(schedStr) {
		if _, _, err := timefmt.ParseSchedule(s); err != nil {
			return fmt.Errorf("invalid schedule %q: %w", s, err)
		}
	}
	return nil
}

// IsScheduleDue checks if ANY schedule time has passed today AND last_executed
// was before that time. Uses local timezone (respects TZ env var).
func IsScheduleDue(schedules []string, lastExecuted time.Time) bool {
	now := time.Now()
	for _, s := range schedules {
		h, m, err := timefmt.ParseSchedule(s)
		if err != nil {
			continue
		}
		todayTarget := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		if now.After(todayTarget) && lastExecuted.Before(todayTarget) {
			return true
		}
	}
	return false
}
