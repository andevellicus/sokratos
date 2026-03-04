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

// supervisorMaxTokens is the max_tokens value sent with every supervisor
// ChatRequest. Prevents llama-server from truncating responses silently.
const supervisorMaxTokens = 4096

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
var toolIntentCodeRe = regexp.MustCompile(`(?s)<TOOL_INTENT>(.*?</CODE>)\s*(?:</TOOL_INTENT>)?`)

// closingToolIntentRe matches closing </TOOL_INTENT> tags. Used to strip
// stray closing tags from the intent after CODE block removal — when the
// model places </TOOL_INTENT> before <CODE>, the code-block regex captures
// the inner closing tag as part of the content.
var closingToolIntentRe = regexp.MustCompile(`</TOOL_INTENT>`)

// toolIntentRe matches <TOOL_INTENT>...</TOOL_INTENT> for intents without
// a <CODE> block.
var toolIntentRe = regexp.MustCompile(`(?s)<TOOL_INTENT>(.*?)</TOOL_INTENT>`)

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
// Identity card and personality are injected between the core rules and tool
// descriptions so the model sees them early in the prompt.
func buildSupervisorSystemPrompt(toolDescs, profileContent, personalityContent string) string {
	// Start with the base system prompt.
	sp := systemPromptBase

	// Identity card goes right after the core rules — before tools and
	// everything else — so even small models pay attention to it.
	if profileContent != "" {
		sp += "\n\n## Identity Card\n" + profileContent
	}
	if personalityContent != "" {
		sp += "\n\n" + personalityContent
	}

	// Tool descriptions follow after identity/personality.
	sp += "\n\n" + strings.TrimSpace(toolDescs)

	// Replace tool-call format placeholder with TOOL_INTENT tag instructions.
	sp = strings.Replace(sp,
		"- Call ONE tool per turn. You may include reasoning and context alongside your tool call.",
		"- When you need to use a tool, wrap your intent in XML tags. You MUST use the closing tag </TOOL_INTENT> (with a forward slash):\n  <TOOL_INTENT>tool_name: {\"param\": \"value\"}</TOOL_INTENT>\n- ALWAYS include the arguments JSON object, even if empty: <TOOL_INTENT>tool_name: {}</TOOL_INTENT>\n- Call ONE tool per turn. You may include brief context outside the tags.\n- CRITICAL: Any text alongside a TOOL_INTENT IS sent to the user immediately. After the tool executes you get a follow-up turn. In that turn, do NOT repeat or rephrase what you already said. Keep the follow-up to ONE short sentence confirming the result. If your pre-tool text already covered everything, just confirm with a brief acknowledgment like \"Done.\" or the key fact from the result.",
		1)

	// Replace idle instruction with plain text idle.
	sp = strings.Replace(sp,
		"Otherwise output: <NO_ACTION_REQUIRED>",
		"Otherwise just respond with: idle",
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
	var profileContent, personalityContent string
	if opts != nil {
		profileContent = opts.ProfileContent
		personalityContent = opts.PersonalityContent
	}
	sysContent := buildSupervisorSystemPrompt(toolDescs, profileContent, personalityContent)

	// Replace the %MAX_WEB_SOURCES% placeholder.
	maxWeb := "2"
	if opts != nil && opts.MaxWebSources > 0 {
		maxWeb = strconv.Itoa(opts.MaxWebSources)
	}
	sysContent = strings.Replace(sysContent, "%MAX_WEB_SOURCES%", maxWeb, 1)
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

	// callTool wraps toolExec with slot release/reacquire callbacks.
	callTool := func(ctx context.Context, raw []byte) (string, error) {
		if opts != nil && opts.OnToolStart != nil {
			opts.OnToolStart()
		}
		result, err := toolExec(ctx, raw)
		if opts != nil && opts.OnToolEnd != nil {
			if reErr := opts.OnToolEnd(ctx); reErr != nil {
				logger.Log.Warnf("[llm:supervisor] slot reacquire failed: %v", reErr)
			}
		}
		return result, err
	}

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
			Model:              model,
			Messages:           sent,
			MaxTokens:          supervisorMaxTokens,
			ChatTemplateKwargs: map[string]any{"enable_thinking": false},
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
				toolName, originalArgs := extractToolNameAndArgs(toolJSON)

				result, execErr := callTool(ctx, []byte(toolJSON))

				// Determine failure and build the failure message.
				var failureMsg string
				isFailed := false
				if execErr != nil {
					failureMsg = execErr.Error()
					isFailed = true
				} else if IsToolSoftError(result) {
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
						fbResult, fbErr := callTool(ctx, []byte(fbJSON))
						if fbErr != nil {
							messages = append(messages, Message{Role: "user", Content: fmt.Sprintf(
								"Tool result [auto-fallback]: %s failed (%s). Fallback to %s also failed: %s",
								toolName, failureMsg, fb.FallbackTool, fbErr.Error())})
						} else {
							fbResult = textutil.TruncateToolResult(fbResult, resolveToolResultLen(opts), "")
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
						result = textutil.TruncateToolResult(result, resolveToolResultLen(opts), "")
						messages = append(messages, Message{Role: "user", Content: "Tool result: " + result + hint})
					}
					continue
				}

				// Success path.
				result = textutil.TruncateToolResult(result, resolveToolResultLen(opts), "Use specific queries or filters to narrow results")
				messages = append(messages, Message{Role: "user", Content: "Tool result: " + result})
				continue
			}
		} else if resp.FinishReason == "length" && strings.Contains(content, "<TOOL_INTENT>") {
			// Response was truncated mid-tag — retry with a nudge.
			logger.Log.Warnf("[llm:supervisor] response truncated mid-TOOL_INTENT, requesting retry")
			messages = append(messages, Message{Role: "assistant", Content: raw})
			messages = append(messages, Message{Role: "user", Content: "Your response was cut off mid-tag. Rewrite your TOOL_INTENT — keep any reasoning shorter."})
			continue
		}

		// No tool intent — this is the final response.
		messages = append(messages, Message{Role: "assistant", Content: raw})
		return content, messages[historyLen:], nil
	}

	return "", messages[historyLen:], fmt.Errorf("too many tool call rounds")
}
