package orchestrate

import (
	"encoding/json"
	"strings"
)

// softErrorPatterns are substrings that indicate a tool returned a soft error
// (user-facing failure message returned as result string, not a Go error).
var softErrorPatterns = []string{
	"error", "failed", "timeout", "deadline exceeded", "unavailable",
	"not found", "no results", "could not",
}

// IsToolSoftError returns true when a tool result string indicates a
// user-facing failure (soft error convention: return "error message", nil).
// Structured data (JSON objects/arrays, count-prefixed results) is never
// treated as a soft error, even if the content happens to contain words
// like "error" or "failed" in news headlines or article summaries.
func IsToolSoftError(result string) bool {
	trimmed := strings.TrimSpace(result)
	if len(trimmed) == 0 {
		return false
	}
	// Structured data is never a soft error.
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return false
	}
	// Only check the first 200 characters — error messages are short,
	// but long tool results may contain trigger words in body content.
	lower := strings.ToLower(trimmed)
	if len(lower) > 200 {
		lower = lower[:200]
	}
	for _, pat := range softErrorPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// matchFallback checks whether a failed tool has a configured fallback and
// whether the failure message matches the trigger pattern (if any).
func matchFallback(fallbacks FallbackMap, toolName, failureMsg string) (FallbackDef, bool) {
	if fallbacks == nil {
		return FallbackDef{}, false
	}
	fb, ok := fallbacks[toolName]
	if !ok {
		return FallbackDef{}, false
	}
	if fb.TriggerPattern != nil && !fb.TriggerPattern.MatchString(failureMsg) {
		return FallbackDef{}, false
	}
	return fb, true
}

// buildToolJSON constructs a canonical tool-call JSON string from name and args.
func buildToolJSON(name string, args json.RawMessage) string {
	tc := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: name, Arguments: args}
	b, _ := json.Marshal(tc)
	return string(b)
}

// toolHint returns a brief contextual hint for tool errors where no fallback
// is configured, helping the orchestrator recover without wasting rounds.
func toolHint(toolName string) string {
	if strings.HasPrefix(toolName, "get-") || strings.HasPrefix(toolName, "twitter-") {
		return "\nHint: consider using search_web as a fallback."
	}
	switch toolName {
	case "search_email", "search_calendar":
		return "\nHint: try broadening the query or adjusting time bounds."
	case "search_memory":
		return "\nHint: try different keywords or broader terms."
	}
	return ""
}
