package routines

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const timeLayout = "2006-01-02T15:04:05"

// templateRe matches {{base}} or {{base+offset}} or {{base-offset}}
// where base is a keyword (today, tomorrow, yesterday, now) and offset
// is a number followed by a unit (m, h, d, w).
var templateRe = regexp.MustCompile(`\{\{(today|tomorrow|yesterday|now)([+-]\d+[mhdw])?\}\}`)

// baseTime resolves a keyword to its base time value.
func baseTime(keyword string, now time.Time) time.Time {
	switch keyword {
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "tomorrow":
		tom := now.AddDate(0, 0, 1)
		return time.Date(tom.Year(), tom.Month(), tom.Day(), 0, 0, 0, 0, now.Location())
	case "yesterday":
		yes := now.AddDate(0, 0, -1)
		return time.Date(yes.Year(), yes.Month(), yes.Day(), 0, 0, 0, 0, now.Location())
	case "now":
		return now
	default:
		return now
	}
}

// applyOffset parses an offset string like "+2h", "-3d", "+30m", "-1w"
// and applies it to t. Returns t unchanged if offset is empty or malformed.
func applyOffset(t time.Time, offset string) time.Time {
	if offset == "" {
		return t
	}

	sign := 1
	if offset[0] == '-' {
		sign = -1
	}
	// Strip the +/- prefix.
	numUnit := offset[1:]
	unit := numUnit[len(numUnit)-1]
	n, err := strconv.Atoi(numUnit[:len(numUnit)-1])
	if err != nil {
		return t
	}
	n *= sign

	switch unit {
	case 'm':
		return t.Add(time.Duration(n) * time.Minute)
	case 'h':
		return t.Add(time.Duration(n) * time.Hour)
	case 'd':
		return t.AddDate(0, 0, n)
	case 'w':
		return t.AddDate(0, 0, n*7)
	default:
		return t
	}
}

// ExpandArgs walks a map and replaces template expressions in string values.
// Non-string values (numbers, booleans, nested objects) pass through unchanged.
//
// Supported templates:
//
//	{{today}}          → today 00:00
//	{{tomorrow}}       → tomorrow 00:00
//	{{yesterday}}      → yesterday 00:00
//	{{now}}            → current time
//	{{base+offset}}    → base time + offset (e.g. {{now-2h}}, {{today+3d}})
//
// Offset units: m (minutes), h (hours), d (days), w (weeks).
// All times are formatted as 2006-01-02T15:04:05 in local timezone.
func ExpandArgs(args map[string]interface{}) map[string]interface{} {
	if args == nil {
		return nil
	}
	now := time.Now()
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok {
			out[k] = expandString(s, now)
		} else {
			out[k] = v
		}
	}
	return out
}

// expandString replaces all template expressions in a string.
func expandString(s string, now time.Time) string {
	return templateRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := templateRe.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		t := baseTime(parts[1], now)
		if len(parts) >= 3 && parts[2] != "" {
			t = applyOffset(t, parts[2])
		}
		return t.Format(timeLayout)
	})
}

// ExpandAndMarshal unmarshals raw JSON args, expands template expressions in
// string values, and re-marshals to JSON. Returns the original on any error.
func ExpandAndMarshal(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	expanded := ExpandArgs(m)
	out, err := json.Marshal(expanded)
	if err != nil {
		return raw
	}
	return out
}

// ExpandString is the exported version of expandString for use outside the
// package (e.g. formatting routine descriptions). Expands all template
// expressions using the current time.
func ExpandString(s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return expandString(s, time.Now())
}
