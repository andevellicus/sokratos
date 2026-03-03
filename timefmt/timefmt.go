package timefmt

import (
	"fmt"
	"time"
)

// Standard time format layouts used across the codebase.
const (
	// DateTime is the standard display format for timestamps that need both
	// date and time (archives, emails, memories, events, heartbeat context).
	DateTime = "2006-01-02 15:04"

	// DateOnly is used for date-without-time contexts (all-day calendar
	// events, memory creation dates).
	DateOnly = "2006-01-02"

	// Natural is a human-readable format injected as a time capstone into
	// LLM context so the orchestrator knows the current time.
	Natural = "Monday, January 2, 2006 at 3:04 PM"

	// LogFile is the format used for log file names on disk.
	LogFile = "2006-01-02_15-04-05"
)

// Now returns time.Now() formatted with DateTime.
func Now() string { return time.Now().Format(DateTime) }

// FormatDateTime formats t with the standard DateTime layout.
func FormatDateTime(t time.Time) string { return t.Format(DateTime) }

// FormatDate formats t with the DateOnly layout.
func FormatDate(t time.Time) string { return t.Format(DateOnly) }

// FormatNatural formats t with the Natural layout (for LLM capstones).
func FormatNatural(t time.Time) string { return t.Format(Natural) }

// iso8601Layouts are timezone-less fallback layouts tried by ParseISO8601
// after RFC3339 fails. Interpreted as local time since the orchestrator
// generates timestamps in the user's timezone.
var iso8601Layouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	DateOnly,
}

// ParseISO8601 parses a time string trying RFC3339 first, then common
// ISO 8601 variants without timezone info. Timezone-less formats are
// interpreted as local time (not UTC).
func ParseISO8601(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range iso8601Layouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("parsing time %q: unrecognized format", s)
}

// ParseSchedule validates an "HH:MM" string and returns hour, minute.
// Used by routine sync, routine management, and schedule evaluation.
func ParseSchedule(s string) (int, int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("invalid schedule format %q (expected HH:MM)", s)
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid schedule %q: hour must be 0-23, minute 0-59", s)
	}
	return h, m, nil
}

// ReinterpretAsLocal takes a time and, if it's in UTC, reinterprets the wall
// clock values as local time. This handles the common case where an LLM
// appends "Z" to timestamps but actually means local time. If the time already
// has a non-UTC zone (e.g. "-05:00"), it is returned unchanged.
func ReinterpretAsLocal(t time.Time) time.Time {
	if t.Location() != time.UTC {
		return t
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
}
