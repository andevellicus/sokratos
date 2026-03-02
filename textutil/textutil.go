package textutil

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Truncate returns s unchanged if len(s) <= n, otherwise truncates to at most
// n bytes at a valid UTF-8 rune boundary and appends "...".
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back off to avoid splitting a multi-byte UTF-8 character.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// thinkTagRe matches <think>...</think> blocks (including across newlines).
var thinkTagRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

// thinkContentRe captures the content inside <think>...</think> blocks.
var thinkContentRe = regexp.MustCompile(`(?s)<think>(.*?)</think>`)

// StripThinkTags removes reasoning blocks from LLM output. Handles two patterns:
// 1. Matched <think>...</think> pairs (Qwen3 style)
// 2. Orphaned </think> where <think> was injected by the chat template:
//    the response contains "[reasoning]</think>[actual content]" — strip up to </think>.
func StripThinkTags(s string) string {
	// First: strip matched <think>...</think> pairs.
	result := thinkTagRe.ReplaceAllString(s, "")
	// Then: handle orphaned </think> from template-injected <think>.
	// Everything before the last </think> is reasoning preamble.
	if idx := strings.LastIndex(result, "</think>"); idx != -1 {
		result = result[idx+len("</think>"):]
	}
	return strings.TrimSpace(result)
}

// ExtractThinkContent returns the concatenated content of all <think> blocks
// in s. Returns empty string if no think blocks are present.
func ExtractThinkContent(s string) string {
	matches := thinkContentRe.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return ""
	}
	var parts []string
	for _, m := range matches {
		if t := strings.TrimSpace(m[1]); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// codeFenceRe matches a markdown code fence block anywhere in a string.
// Captures: opening fence line (with optional language tag) + inner content.
var codeFenceRe = regexp.MustCompile("(?s)```[a-zA-Z]*\\s*\n(.*?)```")

// StripCodeFences removes markdown code fences (```json ... ``` or ``` ... ```).
// Handles fences both at the start of a string and embedded after prose text.
func StripCodeFences(s string) string {
	// Fast path: starts with fence.
	if strings.HasPrefix(s, "```") {
		inner := s
		if nl := strings.Index(inner, "\n"); nl != -1 {
			inner = inner[nl+1:]
		}
		if idx := strings.LastIndex(inner, "```"); idx != -1 {
			inner = inner[:idx]
		}
		return strings.TrimSpace(inner)
	}
	// Embedded fence: extract the content of the first fenced block.
	if m := codeFenceRe.FindStringSubmatch(s); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return s
}

// toolIntentTagRe matches <TOOL_INTENT>...</TOOL_INTENT> blocks, including
// nested <CODE>...</CODE> blocks used by create_skill.
var toolIntentTagRe = regexp.MustCompile(`(?s)<TOOL_INTENT>.*?</TOOL_INTENT>`)

// StripToolIntentTags removes <TOOL_INTENT>...</TOOL_INTENT> blocks from text.
func StripToolIntentTags(s string) string {
	return strings.TrimSpace(toolIntentTagRe.ReplaceAllString(s, ""))
}

// trailingDotRe matches trailing decimal points (e.g., "salience": 7. }) which are
// technically valid in some JS engines but rejected by Go's strict json.Unmarshal.
var trailingDotRe = regexp.MustCompile(`(\d)\.\s*([,\]}])`)

// decimalSpaceRe matches spaces inside decimal numbers (e.g., "salience": 7. 0)
// which Flash models sometimes produce. Go's json.Unmarshal rejects these.
var decimalSpaceRe = regexp.MustCompile(`(\d\.)\s+(\d)`)

// trailingCommaRe matches trailing commas before closing brackets/braces.
// Common LLM mistake: {"a": 1, "b": 2,} or ["x", "y",].
var trailingCommaRe = regexp.MustCompile(`,\s*([\]}])`)

// CleanLLMJSON applies the standard cleanup pipeline for LLM-generated JSON:
// strip think tags → strip code fences → extract JSON object → clean trailing
// dots → clean trailing commas.
func CleanLLMJSON(s string) string {
	jsonStr := ExtractJSON(StripCodeFences(StripThinkTags(s)))
	jsonStr = decimalSpaceRe.ReplaceAllString(jsonStr, "$1$2")
	jsonStr = trailingDotRe.ReplaceAllString(jsonStr, "$1$2")
	return trailingCommaRe.ReplaceAllString(jsonStr, "$1")
}

// ExtractJSON finds the first top-level JSON object in s by locating the first
// '{' and its matching '}'. Handles the common case where a thinking model
// outputs free-form reasoning before/after the JSON. On truncated JSON (depth
// never returns to 0), returns the partial match from '{' to end-of-string so
// downstream parsers see JSON-like content rather than leading prose.
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
	// Truncated JSON: return from first '{' to end (better than full prose).
	return s[start:]
}
