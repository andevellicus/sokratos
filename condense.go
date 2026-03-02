package main

import (
	"regexp"
	"strings"

	"sokratos/llm"
	"sokratos/textutil"
)

// toolNameFromIntentRe extracts the tool name from a <TOOL_INTENT>tool_name: ...
// tag in an assistant message.
var toolNameFromIntentRe = regexp.MustCompile(`<TOOL_INTENT>\s*(\w+)\s*:`)

const maxCondensedLen = 400
// summarizeToolContext extracts tool calls and their results from orchestrator
// messages for inclusion in the triage exchange. No aggressive truncation — the
// triage pipeline's MaxTriageLen handles the overall budget, and the subagent
// has a 16K context window.
func summarizeToolContext(msgs []llm.Message) (string, bool) {
	var sb strings.Builder
	found := false

	for i, m := range msgs {
		if m.Role != "user" || !strings.HasPrefix(m.Content, "Tool result: ") {
			continue
		}
		found = true

		toolName := "tool"
		if i > 0 && msgs[i-1].Role == "assistant" {
			if match := toolNameFromIntentRe.FindStringSubmatch(msgs[i-1].Content); len(match) >= 2 {
				toolName = match[1]
			}
		}

		body := strings.TrimPrefix(m.Content, "Tool result: ")
		sb.WriteString("[" + toolName + " result] " + body + "\n")
	}

	if !found {
		return "", false
	}
	return sb.String(), true
}

// condenseToolResults replaces intermediate tool results with shortened
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

		body := strings.TrimPrefix(m.Content, "Tool result: ")

		// Short results: keep as-is.
		if len(body) <= maxCondensedLen {
			continue
		}

		// Truncate to maxCondensedLen, preserving enough content for factual
		// grounding in follow-up exchanges (e.g. multiple search result titles).
		condensed := textutil.Truncate(body, maxCondensedLen)

		out[i] = llm.Message{
			Role:    m.Role,
			Content: "Tool result: [" + toolName + "] " + condensed,
			Time:    m.Time,
		}
	}

	return out
}
