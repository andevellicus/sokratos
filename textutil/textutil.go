package textutil

import (
	"regexp"
	"strings"
)

// thinkTagRe matches <think>...</think> blocks (including across newlines).
var thinkTagRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

// StripThinkTags removes Qwen3-style <think>...</think> reasoning blocks
// from the response so they are never shown to users or parsed as tool calls.
// Uses regex for multiline safety and handles nested tags.
func StripThinkTags(s string) string {
	return strings.TrimSpace(thinkTagRe.ReplaceAllString(s, ""))
}

// StripCodeFences removes markdown code fences (```json ... ``` or ``` ... ```).
// Uses LastIndex for the closing fence to handle content containing triple backticks.
func StripCodeFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Remove opening fence line.
	if nl := strings.Index(s, "\n"); nl != -1 {
		s = s[nl+1:]
	}
	// Remove closing fence.
	if idx := strings.LastIndex(s, "```"); idx != -1 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// toolIntentTagRe matches <TOOL_INTENT>...</TOOL_INTENT> blocks, including
// nested <CODE>...</CODE> blocks used by create_skill.
var toolIntentTagRe = regexp.MustCompile(`(?s)<TOOL_INTENT>.*?</TOOL_INTENT>`)

// StripToolIntentTags removes <TOOL_INTENT>...</TOOL_INTENT> blocks from text.
func StripToolIntentTags(s string) string {
	return strings.TrimSpace(toolIntentTagRe.ReplaceAllString(s, ""))
}

// CleanLLMJSON applies the standard cleanup pipeline for LLM-generated JSON:
// strip think tags → strip code fences → extract JSON object.
func CleanLLMJSON(s string) string {
	return ExtractJSON(StripCodeFences(StripThinkTags(s)))
}

// ExtractJSON finds the first top-level JSON object in s by locating the first
// '{' and its matching '}'. Handles the common case where a thinking model
// outputs free-form reasoning before/after the JSON.
func ExtractJSON(s string) string {
	start := strings.Index(s, "{")
	if start == -1 {
		return s
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s
}
