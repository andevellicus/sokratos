package main

import (
	"regexp"
	"strings"

	"sokratos/llm"
)

// toolNameFromIntentRe extracts the tool name from a <TOOL_INTENT>tool_name: ...
// tag in an assistant message.
var toolNameFromIntentRe = regexp.MustCompile(`<TOOL_INTENT>\s*(\w+)\s*:`)

const maxCondenseFirstLine = 120

// condenseToolResults replaces intermediate tool results with breadcrumb
// summaries to reduce context window bloat. A tool result is "intermediate"
// when a later assistant message exists (the orchestrator already synthesized
// it). The last tool result is preserved in full since it may still be
// actively referenced.
//
// "Tool error:" messages are never condensed.
// The "Tool result: " prefix is preserved (required by isToolMessage in engine/trim.go).
func condenseToolResults(msgs []llm.Message) []llm.Message {
	// Find last assistant message index.
	lastAssistant := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			lastAssistant = i
			break
		}
	}
	if lastAssistant < 0 {
		return msgs
	}

	out := make([]llm.Message, len(msgs))
	copy(out, msgs)

	for i, m := range out {
		// Only condense tool results that precede the last assistant message.
		if i >= lastAssistant {
			break
		}
		if m.Role != "user" || !strings.HasPrefix(m.Content, "Tool result: ") {
			continue
		}
		// Don't condense error messages.
		if strings.HasPrefix(m.Content, "Tool error: ") {
			continue
		}

		// Extract tool name from the preceding assistant message.
		toolName := "unknown"
		if i > 0 && out[i-1].Role == "assistant" {
			if match := toolNameFromIntentRe.FindStringSubmatch(out[i-1].Content); len(match) >= 2 {
				toolName = match[1]
			}
		}

		// Get first line of the result (after "Tool result: " prefix).
		body := strings.TrimPrefix(m.Content, "Tool result: ")
		firstLine := body
		if idx := strings.IndexByte(body, '\n'); idx >= 0 {
			firstLine = body[:idx]
		}
		if len(firstLine) > maxCondenseFirstLine {
			firstLine = firstLine[:maxCondenseFirstLine] + "..."
		}

		out[i] = llm.Message{
			Role:    m.Role,
			Content: "Tool result: [" + toolName + " -> " + firstLine + "]",
			Time:    m.Time,
		}
	}

	return out
}
