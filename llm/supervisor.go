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

// streamPhase tracks which phase the streaming state machine is in.
type streamPhase int

const (
	phaseThink  streamPhase = iota // buffering inside <think>...</think>
	phaseProbe                     // buffering first N chars post-think, checking for <TOOL_INTENT
	phaseStream                    // forwarding tokens to the callback
	phaseMuted                     // tool intent detected, stop forwarding
)

const probeChars = 50 // chars to buffer in probe phase before committing to stream

// streamState is a state machine that decides which tokens from the LLM stream
// should be forwarded to the user via the OnStreamChunk callback. It handles:
// - Suppressing <think>...</think> blocks
// - Probing the first few chars after think to detect tool intents
// - Forwarding user-visible content
// - Muting once a <TOOL_INTENT> tag is detected
type streamState struct {
	callback  func(token string)
	phase     streamPhase
	acc       strings.Builder // full accumulated output (including think blocks)
	postThink strings.Builder // accumulated content after </think>
}

// newStreamState creates a stream state machine with the given callback.
func newStreamState(callback func(token string)) *streamState {
	return &streamState{
		callback: callback,
		phase:    phaseThink,
	}
}

// onToken processes a single token from the LLM stream. Returns false to abort
// the stream (when tool intent is fully detected in muted phase).
func (ss *streamState) onToken(token string) bool {
	ss.acc.WriteString(token)
	full := ss.acc.String()

	switch ss.phase {
	case phaseThink:
		// Check if we've exited the think block.
		if strings.Contains(full, "</think>") {
			// Extract everything after the last </think>.
			idx := strings.LastIndex(full, "</think>")
			post := full[idx+len("</think>"):]
			ss.postThink.WriteString(post)
			ss.phase = phaseProbe
			// Fall through to probe logic.
			return ss.probeCheck()
		}
		// Still in think block. But if there's no <think> tag at all and we
		// have some content, the model may not be using think blocks this round.
		if !strings.Contains(full, "<think>") && len(full) > 20 {
			// No think block — treat everything as post-think.
			ss.postThink.WriteString(full)
			ss.phase = phaseProbe
			return ss.probeCheck()
		}
		return true

	case phaseProbe:
		ss.postThink.WriteString(token)
		return ss.probeCheck()

	case phaseStream:
		ss.postThink.WriteString(token)
		ss.callback(token)
		// Late tool intent check (rare but possible).
		if strings.Contains(ss.postThink.String(), "<TOOL_INTENT") {
			ss.phase = phaseMuted
			return true // keep reading to get the full intent
		}
		return true

	case phaseMuted:
		// Keep accumulating until the full intent is captured.
		// Check for closing tag to abort early.
		if strings.Contains(full, "</TOOL_INTENT>") || strings.Contains(full, "</CODE>") {
			return false // abort stream, we have the full intent
		}
		return true
	}

	return true
}

// probeCheck is called during the probe phase to decide whether to transition
// to streaming or muting.
func (ss *streamState) probeCheck() bool {
	post := ss.postThink.String()

	// Check for tool intent in buffered content.
	if strings.Contains(post, "<TOOL_INTENT") {
		ss.phase = phaseMuted
		return true // keep reading to get the full intent
	}

	// If we've buffered enough without seeing a tool intent, start streaming.
	if len(post) >= probeChars {
		ss.phase = phaseStream
		// Flush the buffered post-think content.
		trimmed := strings.TrimLeft(post, " \t\n\r")
		if trimmed != "" {
			ss.callback(trimmed)
		}
		return true
	}

	return true
}

// streamed returns true if the state machine reached the streaming phase
// (meaning some content was sent to the user).
func (ss *streamState) streamed() bool {
	return ss.phase == phaseStream || (ss.phase == phaseMuted && ss.postThink.Len() >= probeChars)
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

		// Call orchestrator WITHOUT grammar. Use streaming when a callback
		// is provided so the user sees tokens progressively.
		chatReq := ChatRequest{
			Model:    model,
			Messages: sent,
		}
		var resp ChatResult
		var ss *streamState
		if opts != nil && opts.OnStreamChunk != nil {
			ss = newStreamState(opts.OnStreamChunk)
			var sErr error
			resp, sErr = client.ChatStream(ctx, chatReq, ss.onToken)
			if sErr != nil {
				return "", messages[historyLen:], sErr
			}
		} else {
			var cErr error
			resp, cErr = client.Chat(ctx, chatReq)
			if cErr != nil {
				return "", messages[historyLen:], cErr
			}
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
				result, execErr := toolExec(ctx, []byte(toolJSON))
				if execErr != nil {
					messages = append(messages, Message{Role: "user", Content: "Tool error: " + execErr.Error()})
				} else {
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
