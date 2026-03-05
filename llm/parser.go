package llm

import "strings"

// ParseResult describes the outcome of parsing model output for a tool call.
type ParseResult struct {
	ToolCallJSON string // canonical {"name":"...","arguments":{...}} JSON
	TextBefore   string // text the model emitted before the tool call (for logging)
	Found        bool   // true if a valid tool call was parsed
	Error        string // non-empty if parsing failed (caller should retry)
	BareIntent   bool   // true if bare tool name without args (caller should nudge)
}

// ToolIntentParser parses model output to extract tool calls. Different model
// backends can implement this interface to support their own tool-call format.
type ToolIntentParser interface {
	Parse(output string) ParseResult
}

// SupervisorParser implements ToolIntentParser for the <TOOL_INTENT> tag
// format used by the supervisor pattern.
type SupervisorParser struct{}

// Parse extracts a tool call from model output using the <TOOL_INTENT> tag
// format. Composes extractToolIntent + parseToolIntent.
func (SupervisorParser) Parse(output string) ParseResult {
	intent, ok := extractToolIntent(output)
	if !ok {
		return ParseResult{}
	}

	// Bare tool name without arguments — nudge for proper format.
	if !strings.Contains(intent, ":") && !strings.Contains(intent, "{") {
		return ParseResult{BareIntent: true, Found: true}
	}

	toolJSON, parsed := parseToolIntent(intent)
	if !parsed {
		return ParseResult{Found: true, Error: toolJSON}
	}
	return ParseResult{ToolCallJSON: toolJSON, Found: true}
}
