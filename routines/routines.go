package routines

import (
	"encoding/json"
	"strings"
	"time"
)

// Entry is the unified routine definition — used for TOML, JSON, and DB representation.
type Entry struct {
	Interval      string      `toml:"interval,omitempty"        json:"interval,omitempty"`
	Schedule      interface{} `toml:"schedule,omitempty"        json:"schedule,omitempty"` // string or []string
	Tool          string      `toml:"tool,omitempty"            json:"tool,omitempty"`
	Tools         []string    `toml:"tools,omitempty"           json:"tools,omitempty"`
	ToolArgs      map[string]map[string]interface{} `toml:"tool_args,omitempty" json:"tool_args,omitempty"`
	Goal          string      `toml:"goal,omitempty"            json:"goal,omitempty"`
	SilentIfEmpty bool        `toml:"silent_if_empty,omitempty" json:"silent_if_empty,omitempty"`
	Instruction   string      `toml:"instruction,omitempty"     json:"instruction,omitempty"`
}

// DueRoutine represents a routine row that's a candidate for execution.
type DueRoutine struct {
	ID            int
	Name          string
	Instruction   string
	Tool          *string
	Tools         []string
	ToolArgs      map[string]json.RawMessage // tool_name → JSON args (from JSONB)
	Goal          *string
	SilentIfEmpty bool
	Schedules     []string // parsed from comma-separated schedule column
	LastExecuted  time.Time
}

// NilIfEmpty returns nil for empty strings (maps to SQL NULL).
func NilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// IsEmptyResult checks if a tool result indicates no data was returned.
func IsEmptyResult(result string) bool {
	if result == "" {
		return true
	}
	// Skills return "No tweets found.", "No news articles found.", etc.
	for _, prefix := range []string{"No ", "no ", "error", "Error"} {
		if len(result) > len(prefix) && result[:len(prefix)] == prefix {
			return true
		}
	}
	// JSON results with count=0
	if len(result) > 10 && result[0] == '{' {
		if strings.Contains(result, `"count":0`) {
			return true
		}
	}
	return false
}
