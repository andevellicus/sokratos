package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timefmt"
)

// toolIntentCodeRe matches <TOOL_INTENT>...<CODE>...</CODE> with an optional
// </TOOL_INTENT> after it. The captured group INCLUDES the </CODE> tag so
// parseToolIntent can extract the code block properly.
var toolIntentCodeRe = regexp.MustCompile(`(?s)<TOOL_INTENT>(.*?</CODE>)\s*(?:<[/\\]?TOOL_INT[A-Z]*>)?`)

// closingToolIntentRe matches closing TOOL_INTENT tags (including common
// model mistakes). Used to strip stray closing tags from the intent after
// CODE block removal — when the model places </TOOL_INTENT> before <CODE>,
// the code-block regex captures the inner closing tag as part of the content.
var closingToolIntentRe = regexp.MustCompile(`<[/\\]TOOL_INT[A-Z]*>`)

// toolIntentRe matches <TOOL_INTENT>...</TOOL_INTENT> for intents without
// a <CODE> block, including common model mistakes (backslash closer,
// truncated tags, etc.).
var toolIntentRe = regexp.MustCompile(`(?s)<TOOL_INTENT>(.*?)<[/\\]?TOOL_INT[A-Z]*>`)

// extractToolIntent extracts the content of the first <TOOL_INTENT> tag.
// It tries the CODE-block pattern first, then falls back to the simple one.
func extractToolIntent(s string) (string, bool) {
	// Try CODE-block pattern first (greedy enough to include </CODE>).
	if m := toolIntentCodeRe.FindStringSubmatch(s); len(m) >= 2 && strings.Contains(m[1], "<CODE>") {
		return strings.TrimSpace(m[1]), true
	}
	// Fallback: simple tool intent without code block.
	if m := toolIntentRe.FindStringSubmatch(s); len(m) >= 2 {
		return strings.TrimSpace(m[1]), true
	}
	return "", false
}

// buildSupervisorSystemPrompt adapts the standard system prompt for the
// supervisor pattern: replaces JSON tool-call instructions with TOOL_INTENT
// tag instructions, injects dynamic tool descriptions, and removes the
// respond meta-tool requirement.
func buildSupervisorSystemPrompt(toolDescs string) string {
	// Start with the base system prompt + dynamic tool descriptions.
	sp := systemPromptBase + "\n\n" + strings.TrimSpace(toolDescs)

	// Replace tool-call JSON format instructions with TOOL_INTENT tag instructions.
	sp = strings.Replace(sp,
		"- Call ONE tool per turn. Output ONLY the JSON object, nothing else:\n  {\"name\":\"<tool_name>\",\"arguments\":{}}\n- No markdown code fences around tool calls.",
		"- When you need to use a tool, wrap your intent in XML tags. You MUST use the closing tag </TOOL_INTENT> (with a forward slash):\n  <TOOL_INTENT>tool_name: {\"param\": \"value\"}</TOOL_INTENT>\n  A dedicated tool agent will translate your intent into a structured call.\n- ALWAYS include the arguments JSON object, even if empty: <TOOL_INTENT>tool_name: {}</TOOL_INTENT>\n- Call ONE tool per turn. You may include reasoning and context outside the tags.",
		1)

	// Replace respond tool instruction with plain text response instruction.
	sp = strings.Replace(sp,
		"use the respond tool to deliver your answer in clear prose. Do not keep calling tools.",
		"respond directly in plain text. Do not wrap your final answer in any tags. Do not keep calling tools.",
		1)

	// Replace idle respond call with plain text idle.
	sp = strings.Replace(sp,
		"respond with {\"name\":\"respond\",\"arguments\":{\"text\":\"idle\"}}.",
		"just respond with: idle",
		1)

	return sp
}

// parseToolIntent parses a "tool_name: {args_json}" intent string directly
// into a structured tool-call JSON string. Returns the JSON and true on success.
// On failure, returns a descriptive error string and false so the orchestrator
// can retry with corrected formatting.
func parseToolIntent(intent string) (string, bool) {
	// Check for a <CODE>...</CODE> block (used by create_skill to avoid
	// embedding JavaScript inside a JSON string value).
	var codeBlock string
	if codeRe := regexp.MustCompile(`(?s)<CODE>(.*?)</CODE>`); codeRe.MatchString(intent) {
		m := codeRe.FindStringSubmatch(intent)
		codeBlock = strings.TrimSpace(m[1])
		// Remove the <CODE>...</CODE> block from the intent so the
		// remainder is clean "tool_name: {json_args}".
		intent = strings.TrimSpace(codeRe.ReplaceAllString(intent, ""))
		// Strip stray closing TOOL_INTENT tags that may remain when the
		// model places </TOOL_INTENT> before the <CODE> block.
		intent = strings.TrimSpace(closingToolIntentRe.ReplaceAllString(intent, ""))
	}

	colonIdx := strings.Index(intent, ":")
	if colonIdx < 0 {
		return "TOOL_INTENT must use format: tool_name: {\"param\": \"value\"}", false
	}

	toolName := strings.TrimSpace(intent[:colonIdx])
	argsRaw := strings.TrimSpace(intent[colonIdx+1:])

	if toolName == "" {
		return "Empty tool name in TOOL_INTENT", false
	}

	// Escape raw control characters (newlines/tabs) that may appear in
	// the JSON args string. This handles LLMs that still put small bits
	// of multiline content directly in the JSON value fields.
	var sanitized strings.Builder
	for i := 0; i < len(argsRaw); i++ {
		switch argsRaw[i] {
		case '\n':
			sanitized.WriteString(`\n`)
		case '\r':
			sanitized.WriteString(`\r`)
		case '\t':
			sanitized.WriteString(`\t`)
		default:
			sanitized.WriteByte(argsRaw[i])
		}
	}
	argsRaw = sanitized.String()

	// Strip trailing '>' that some models add before </TOOL_INTENT>.
	argsRaw = strings.TrimRight(argsRaw, ">")
	argsRaw = strings.TrimSpace(argsRaw)

	// If we extracted a <CODE> block, inject it into the JSON args
	// as the "code" field. The code is properly JSON-escaped so the
	// LLM never has to worry about escaping.
	if codeBlock != "" {
		// Parse the existing args into a map, inject the code, re-marshal.
		var argsMap map[string]interface{}
		if err := json.Unmarshal([]byte(argsRaw), &argsMap); err != nil {
			return fmt.Sprintf("Invalid JSON arguments for %s (before code injection): %v", toolName, err), false
		}
		argsMap["code"] = codeBlock
		injected, err := json.Marshal(argsMap)
		if err != nil {
			return fmt.Sprintf("Failed to inject code into args: %v", err), false
		}
		argsRaw = string(injected)
	}

	// Validate args as JSON.
	if !json.Valid([]byte(argsRaw)) {
		return fmt.Sprintf("Invalid JSON arguments for %s. Ensure your TOOL_INTENT contains valid JSON: <TOOL_INTENT>%s: {\"param\": \"value\"}</TOOL_INTENT>", toolName, toolName), false
	}

	// Construct the canonical tool-call JSON.
	tc := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{
		Name:      toolName,
		Arguments: json.RawMessage(argsRaw),
	}

	result, err := json.Marshal(tc)
	if err != nil {
		return fmt.Sprintf("Failed to marshal tool call: %v", err), false
	}

	return string(result), true
}

// querySupervisor implements the multi-agent supervisor pattern. The
// orchestrator (e.g. Qwen3-VL) runs without grammar and produces free-form
// text. When it wants a tool, it wraps intent in <TOOL_INTENT> tags. A
// dedicated tool agent translates that intent into JSON.
func querySupervisor(ctx context.Context, client *Client, model, prompt string, toolExec func(context.Context, json.RawMessage) (string, error), trimFn func([]Message) []Message, opts *QueryOrchestratorOpts) (string, []Message, error) {
	// Build system prompt with dynamic tool descriptions.
	toolDescs := prompts.Tools
	if opts != nil && opts.ToolAgent != nil && opts.ToolAgent.ToolDescriptions != "" {
		toolDescs = opts.ToolAgent.ToolDescriptions
	}
	sysContent := buildSupervisorSystemPrompt(toolDescs)

	// Replace the %MAX_WEB_SOURCES% placeholder.
	maxWeb := "2"
	if opts != nil && opts.MaxWebSources > 0 {
		maxWeb = strconv.Itoa(opts.MaxWebSources)
	}
	sysContent = strings.Replace(sysContent, "%MAX_WEB_SOURCES%", maxWeb, 1)

	// Personality first (defines who Sokratos is), then user knowledge.
	if opts != nil && opts.PersonalityContent != "" {
		sysContent += "\n\n" + opts.PersonalityContent
	}
	if opts != nil && opts.ProfileContent != "" {
		sysContent += "\n\n## Identity Card\n" + opts.ProfileContent
	}

	// NO /no_think appended — thinking works freely in supervisor mode.

	userMsg := Message{Role: "user", Content: prompt}
	if opts != nil && len(opts.Parts) > 0 {
		userMsg.Parts = opts.Parts
	}

	messages := []Message{{Role: "system", Content: sysContent}}

	if opts != nil && len(opts.History) > 0 {
		messages = append(messages, opts.History...)
	}

	historyLen := len(messages)
	messages = append(messages, userMsg)

	for range maxToolRounds {
		sent := messages
		if trimFn != nil {
			sent = trimFn(messages)
		}
		// Rolling timestamp capstone. Marked as system context so the model
		// doesn't treat it as a user message requiring a response.
		timeCapstone := Message{
			Role:    "user",
			Content: "[SYSTEM CONTEXT — not a user message, do not respond to this] Current time: " + timefmt.FormatNatural(time.Now()),
		}
		sent = append(sent, timeCapstone)

		// Call orchestrator WITHOUT grammar.
		resp, err := client.Chat(ctx, ChatRequest{
			Model:    model,
			Messages: sent,
		})
		if err != nil {
			return "", messages[historyLen:], err
		}

		raw := strings.TrimSpace(resp.Message.Content)
		// Strip think tags FIRST, then check for tool intent.
		// This prevents intent inside think blocks from being acted on.
		content := textutil.StripThinkTags(raw)
		logger.Log.Infof("[llm:supervisor] %s", content)

		// Check for tool intent.
		if intent, ok := extractToolIntent(content); ok {
			// If the intent is just a bare tool name without arguments,
			// push back to the orchestrator so it retries with proper args.
			if !strings.Contains(intent, ":") && !strings.Contains(intent, "{") {
				logger.Log.Warnf("[llm:supervisor] bare tool intent %q — requesting retry with arguments", intent)
				messages = append(messages, Message{Role: "assistant", Content: raw})
				messages = append(messages, Message{Role: "user", Content: "Your TOOL_INTENT must include arguments as JSON. Example: <TOOL_INTENT>tool_name: {\"param\": \"value\"}</TOOL_INTENT>. For tools with no arguments, use: <TOOL_INTENT>tool_name: {}</TOOL_INTENT>"})
				continue
			}

			messages = append(messages, Message{Role: "assistant", Content: raw})

			// Parse intent directly — the orchestrator already provides
			// "tool_name: {args_json}" which we can construct into a tool
			// call without an intermediate LLM.
			toolJSON, ok := parseToolIntent(intent)
			if !ok {
				logger.Log.Warnf("[llm:supervisor] intent parse failed: %s", toolJSON)
				messages = append(messages, Message{Role: "user", Content: "Tool call error: " + toolJSON})
				continue
			}

			logger.Log.Infof("[llm:supervisor] parsed tool call: %s", toolJSON)

			// Execute the tool.
			if toolExec != nil {
				result, execErr := toolExec(ctx, []byte(toolJSON))
				if execErr != nil {
					messages = append(messages, Message{Role: "user", Content: "Tool error: " + execErr.Error()})
				} else {
					truncLen := defaultMaxToolResultLen
					if opts != nil && opts.MaxToolResultLen > 0 {
						truncLen = opts.MaxToolResultLen
					}
					if len(result) > truncLen {
						result = result[:truncLen] + "\n... (truncated)"
					}
					messages = append(messages, Message{Role: "user", Content: "Tool result: " + result})
				}
				continue
			}
		}

		// No tool intent — this is the final response.
		messages = append(messages, Message{Role: "assistant", Content: raw})
		return content, messages[historyLen:], nil
	}

	return "", messages[historyLen:], fmt.Errorf("too many tool call rounds")
}
