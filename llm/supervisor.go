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

// softErrorPatterns are substrings that indicate a tool returned a soft error
// (user-facing failure message returned as result string, not a Go error).
var softErrorPatterns = []string{
	"error", "failed", "timeout", "deadline exceeded", "unavailable",
	"not found", "no results", "could not",
}

// isToolSoftError returns true when a tool result string indicates a
// user-facing failure (soft error convention: return "error message", nil).
func isToolSoftError(result string) bool {
	lower := strings.ToLower(result)
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

// extractToolNameAndArgs pulls the tool name and raw arguments from a parsed
// tool-call JSON string. Returns empty values on parse failure.
func extractToolNameAndArgs(toolJSON string) (string, json.RawMessage) {
	var tc struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(toolJSON), &tc); err != nil {
		return "", nil
	}
	return tc.Name, tc.Arguments
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

// stripToolIntentTags removes all <TOOL_INTENT>...</TOOL_INTENT> blocks from
// a string, returning only the surrounding prose. Used to preserve substantive
// text from intermediate supervisor rounds that also contain a tool call.
func stripToolIntentTags(s string) string {
	s = toolIntentCodeRe.ReplaceAllString(s, "")
	s = toolIntentRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

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
		"- When you need to use a tool, wrap your intent in XML tags. You MUST use the closing tag </TOOL_INTENT> (with a forward slash):\n  <TOOL_INTENT>tool_name: {\"param\": \"value\"}</TOOL_INTENT>\n  A dedicated tool agent will translate your intent into a structured call.\n- ALWAYS include the arguments JSON object, even if empty: <TOOL_INTENT>tool_name: {}</TOOL_INTENT>\n- Call ONE tool per turn. You may include reasoning and context outside the tags.\n- IMPORTANT: Any text you write alongside a TOOL_INTENT tag IS shown to the user. After the tool executes, do NOT repeat or rephrase what you already said — only add genuinely new information from the tool result.",
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
	if opts != nil && opts.TemporalContext != "" {
		sysContent += "\n\n" + opts.TemporalContext
	}
	if opts != nil && opts.PrefetchContent != "" {
		sysContent += "\n\n" + opts.PrefetchContent
	}

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

	// Accumulate substantive prose from intermediate rounds that also contain
	// a tool intent. Without this, text like "Ah, my apologies — Clair Obscur
	// is a turn-based RPG..." gets swallowed when it accompanies a save_memory
	// intent, and only the final round's text (often filler) is returned.
	var intermediateText []string

	for range maxToolRounds {
		sent := messages
		if trimFn != nil {
			sent = trimFn(messages)
		}
		// Inject current time into the system prompt so the model has
		// temporal awareness without a fake user message it might respond to.
		sent = append([]Message{}, sent...)
		sent[0] = Message{
			Role:    sent[0].Role,
			Content: sent[0].Content + "\n\nCurrent time: " + timefmt.FormatNatural(time.Now()),
		}

		chatReq := ChatRequest{
			Model:    model,
			Messages: sent,
		}
		resp, cErr := client.Chat(ctx, chatReq)
		if cErr != nil {
			return "", messages[historyLen:], cErr
		}

		raw := strings.TrimSpace(resp.Message.Content)
		// Log thinking content before stripping — valuable for debugging
		// and understanding model reasoning.
		if thinking := textutil.ExtractThinkContent(raw); thinking != "" {
			logger.Log.Infof("[llm:thinking] %s", thinking)
		}
		// Strip think tags FIRST, then check for tool intent.
		// This prevents intent inside think blocks from being acted on.
		content := textutil.StripThinkTags(raw)
		logger.Log.Infof("[llm:supervisor] %s", content)

		// Check for tool intent.
		if intent, ok := extractToolIntent(content); ok {
			// Preserve any substantive prose that accompanies the tool intent.
			if prose := stripToolIntentTags(content); prose != "" {
				intermediateText = append(intermediateText, prose)
			}

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
				toolName, originalArgs := extractToolNameAndArgs(toolJSON)

				// Determine failure and build the failure message.
				var failureMsg string
				isFailed := false
				if execErr != nil {
					failureMsg = execErr.Error()
					isFailed = true
				} else if isToolSoftError(result) {
					failureMsg = result
					isFailed = true
				}

				// Try deterministic fallback if the tool failed.
				var fallbacks FallbackMap
				if opts != nil {
					fallbacks = opts.Fallbacks
				}
				if isFailed {
					if fb, ok := matchFallback(fallbacks, toolName, failureMsg); ok {
						// Execute fallback tool.
						fbArgs := fb.ArgsTransform(toolName, originalArgs, failureMsg)
						fbJSON := buildToolJSON(fb.FallbackTool, fbArgs)
						logger.Log.Infof("[llm:supervisor] auto-fallback: %s failed, trying %s", toolName, fb.FallbackTool)
						fbResult, fbErr := toolExec(ctx, []byte(fbJSON))
						if fbErr != nil {
							messages = append(messages, Message{Role: "user", Content: fmt.Sprintf(
								"Tool result [auto-fallback]: %s failed (%s). Fallback to %s also failed: %s",
								toolName, failureMsg, fb.FallbackTool, fbErr.Error())})
						} else {
							truncLen := defaultMaxToolResultLen
							if opts != nil && opts.MaxToolResultLen > 0 {
								truncLen = opts.MaxToolResultLen
							}
							if len(fbResult) > truncLen {
								origLen := len(fbResult)
								fbResult = fbResult[:truncLen] + fmt.Sprintf(
									"\n... (truncated: showing %d of %d chars)",
									truncLen, origLen)
							}
							messages = append(messages, Message{Role: "user", Content: fmt.Sprintf(
								"Tool result [auto-fallback]: %s failed (%s). Fallback to %s:\n%s",
								toolName, failureMsg, fb.FallbackTool, fbResult)})
						}
						continue
					}

					// No fallback configured — inject hint if available.
					hint := toolHint(toolName)
					if execErr != nil {
						messages = append(messages, Message{Role: "user", Content: "Tool error: " + execErr.Error() + hint})
					} else {
						truncLen := defaultMaxToolResultLen
						if opts != nil && opts.MaxToolResultLen > 0 {
							truncLen = opts.MaxToolResultLen
						}
						if len(result) > truncLen {
							origLen := len(result)
							result = result[:truncLen] + fmt.Sprintf(
								"\n... (truncated: showing %d of %d chars)",
								truncLen, origLen)
						}
						messages = append(messages, Message{Role: "user", Content: "Tool result: " + result + hint})
					}
					continue
				}

				// Success path.
				truncLen := defaultMaxToolResultLen
				if opts != nil && opts.MaxToolResultLen > 0 {
					truncLen = opts.MaxToolResultLen
				}
				if len(result) > truncLen {
					origLen := len(result)
					result = result[:truncLen] + fmt.Sprintf(
						"\n... (truncated: showing %d of %d chars. Use specific queries or filters to narrow results.)",
						truncLen, origLen)
				}
				messages = append(messages, Message{Role: "user", Content: "Tool result: " + result})
				continue
			}
		}

		// No tool intent — this is the final response.
		// Prepend any substantive text from intermediate tool-call rounds
		// so the user sees the full response, not just the final round.
		if len(intermediateText) > 0 {
			accumulated := strings.Join(intermediateText, "\n\n")
			if content != "" {
				content = accumulated + "\n\n" + content
			} else {
				content = accumulated
			}
		}
		messages = append(messages, Message{Role: "assistant", Content: raw})
		return content, messages[historyLen:], nil
	}

	return "", messages[historyLen:], fmt.Errorf("too many tool call rounds")
}
