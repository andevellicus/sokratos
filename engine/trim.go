package engine

import (
	"strings"

	"sokratos/llm"
)

// TrimMessages implements a sliding window over a message slice. It always
// preserves messages[0] (the system prompt) and keeps the last maxTail
// messages. If the calculated cutoff lands on a tool-result message, it
// decrements backwards to avoid splitting a tool-call sequence.
func TrimMessages(messages []llm.Message, maxTail int) []llm.Message {
	// Nothing to trim if the slice already fits.
	if len(messages) <= maxTail+1 {
		return messages
	}

	cutoff := max(1, len(messages)-maxTail)

	// Back up past tool-result messages so we never orphan a tool-call pair.
	for cutoff > 1 && isToolMessage(messages[cutoff]) {
		cutoff--
	}

	trimmed := make([]llm.Message, 0, 1+len(messages)-cutoff)
	trimmed = append(trimmed, messages[0])
	trimmed = append(trimmed, messages[cutoff:]...)
	return trimmed
}

// isToolMessage returns true for messages that are part of a tool-result
// exchange and must not be separated from the preceding tool-call.
func isToolMessage(m llm.Message) bool {
	if m.Role == "tool" {
		return true
	}
	return m.Role == "user" &&
		(strings.HasPrefix(m.Content, "Tool result:") ||
			strings.HasPrefix(m.Content, "Tool error:"))
}
