package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sokratos/httputil"
	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timefmt"
	"sokratos/timeouts"
)

// Client wraps communication with an OpenAI-compatible LLM server (e.g. llama-server).
type Client struct {
	BaseURL        string
	HTTPClient     *http.Client
	EnableThinking bool // When false, appends /no_think to the system prompt (Qwen3).
}

// ContentPart represents one element in the OpenAI vision content array.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL carries an image data-URI or URL for the vision API.
type ImageURL struct {
	URL string `json:"url"`
}

// Message represents a single chat message. When Parts is set (e.g. for
// vision messages), the JSON content field is serialized as an array of
// content parts; otherwise it is a plain string.
type Message struct {
	Role    string        `json:"-"`
	Content string        `json:"-"`
	Parts   []ContentPart `json:"-"`
	Time    time.Time     `json:"-"`
}

// MarshalJSON serializes Message. If Parts is non-empty, content is an array
// of content parts (OpenAI vision format); otherwise it is a plain string.
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Parts) > 0 {
		type wire struct {
			Role    string        `json:"role"`
			Content []ContentPart `json:"content"`
		}
		return json.Marshal(wire{Role: m.Role, Content: m.Parts})
	}
	type wire struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	return json.Marshal(wire{Role: m.Role, Content: m.Content})
}

// UnmarshalJSON handles both plain-string and array content formats.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role

	// Try string first (the common case).
	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = s
		return nil
	}

	// Try array of content parts (vision format).
	var parts []ContentPart
	if err := json.Unmarshal(raw.Content, &parts); err == nil {
		m.Parts = parts
		for _, p := range parts {
			if p.Type == "text" {
				m.Content = p.Text
				break
			}
		}
		return nil
	}

	// Fallback: treat raw content as literal string.
	m.Content = string(raw.Content)
	return nil
}

// ChatRequest is the payload sent to the /v1/chat/completions endpoint.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Grammar  string    `json:"grammar,omitempty"`
}

// chatResponse is the OpenAI-compatible response from /v1/chat/completions.
type chatResponse struct {
	ID      string   `json:"id"`
	Choices []choice `json:"choices"`
}

type choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatResult is the value returned by Chat to callers.
type ChatResult struct {
	Message Message
}

// NewClient returns a Client configured to talk to the given LLM server base URL.
// Thinking mode is enabled by default.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTPClient: httputil.NewClient(timeouts.HTTPSafetyNet),
		EnableThinking: true,
	}
}

// Chat sends a non-streaming chat request and returns the assistant message.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	req.Stream = false

	body, err := json.Marshal(req)
	if err != nil {
		return ChatResult{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return ChatResult{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ChatResult{}, fmt.Errorf("llm server returned status %d", resp.StatusCode)
	}

	var raw chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return ChatResult{}, fmt.Errorf("decode response: %w", err)
	}

	if len(raw.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("llm server returned no choices")
	}

	return ChatResult{Message: raw.Choices[0].Message}, nil
}

const maxToolRounds = 15
const defaultMaxToolResultLen = 2000 // truncate individual tool results to stay within context

var systemPromptBase = strings.TrimSpace(prompts.System)

// ToolAgentConfig holds the configuration for the supervisor pattern. When set,
// the orchestrator produces free-form text with <TOOL_INTENT> tags, and
// parseToolIntent translates intents into structured JSON.
type ToolAgentConfig struct {
	ToolDescriptions string // full tool descriptions for system prompt (static + dynamic skills)
}

// QueryOrchestratorOpts holds optional parameters for QueryOrchestrator.
type QueryOrchestratorOpts struct {
	Parts              []ContentPart    // vision content parts for the user message
	History            []Message        // prior conversation history to prepend
	Grammar            string           // GBNF grammar to constrain output (only applied when thinking is off)
	PersonalityContent string           // personality traits markdown — injected into system prompt before profile
	ProfileContent     string           // identity profile JSON — injected into system prompt if non-empty
	MaxToolResultLen   int              // max chars per tool result (0 = default 2000)
	MaxWebSources      int              // replaces %MAX_WEB_SOURCES% in system prompt (0 = default 2)
	ToolAgent          *ToolAgentConfig // when set, enables the supervisor pattern
}

// QueryOrchestrator sends a prompt to the given model, executing tool calls as
// needed, and returns the final human-readable response text along with the
// NEW messages produced during this call (excluding history). An optional
// trimFn is applied to the full message slice before each LLM call to keep the
// context window within limits.
func QueryOrchestrator(ctx context.Context, client *Client, model, prompt string, toolExec func(context.Context, json.RawMessage) (string, error), trimFn func([]Message) []Message, opts *QueryOrchestratorOpts) (string, []Message, error) {
	if opts != nil && opts.ToolAgent != nil {
		return querySupervisor(ctx, client, model, prompt, toolExec, trimFn, opts)
	}
	// --- legacy path unchanged below ---
	// Legacy path: use static tools.
	sysContent := systemPromptBase + "\n\n" + strings.TrimSpace(prompts.Tools)

	// Replace the %MAX_WEB_SOURCES% placeholder with the configured value.
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
		sysContent += "\n\n## User Knowledge Profile\n" + opts.ProfileContent
	}

	if !client.EnableThinking {
		sysContent += "\n/no_think"
	}

	userMsg := Message{Role: "user", Content: prompt}
	if opts != nil && len(opts.Parts) > 0 {
		userMsg.Parts = opts.Parts
	}

	messages := []Message{{Role: "system", Content: sysContent}}

	// Prepend conversation history so the model sees prior exchanges.
	if opts != nil && len(opts.History) > 0 {
		messages = append(messages, opts.History...)
	}

	historyLen := len(messages) // track where new messages start
	messages = append(messages, userMsg)

	// Only apply grammar when thinking mode is off (thinking produces
	// <think>...</think> blocks that the grammar would reject).
	var grammar string
	if opts != nil && opts.Grammar != "" && !client.EnableThinking {
		grammar = opts.Grammar
	}

	for range maxToolRounds {
		sent := messages
		if trimFn != nil {
			sent = trimFn(messages)
		}
		// Inject a rolling timestamp so the model always knows the
		// current time, even across multi-round tool loops.
		timeCapstone := Message{
			Role:    "user",
			Content: "[SYSTEM] CURRENT TIME: " + timefmt.FormatNatural(time.Now()),
		}
		sent = append(sent, timeCapstone)
		resp, err := client.Chat(ctx, ChatRequest{
			Model:    model,
			Messages: sent,
			Grammar:  grammar,
		})
		if err != nil {
			return "", messages[historyLen:], err
		}

		raw := strings.TrimSpace(resp.Message.Content)
		content := textutil.StripThinkTags(raw)
		logger.Log.Infof("[llm] %s", content)
		stripped := textutil.StripCodeFences(content)

		// Execute only the FIRST tool call. The model may output multiple,
		// but we force one-at-a-time so it sees results before deciding
		// the next step (critical for gather → brief → respond pipeline).
		if toolJSON, ok := extractToolCall(stripped); ok {
			// Handle the "respond" meta-tool: extract text as final answer.
			if text, isRespond := extractRespondText(toolJSON); isRespond {
				messages = append(messages, Message{Role: "assistant", Content: raw})
				return text, messages[historyLen:], nil
			}

			if toolExec != nil {
				result, execErr := toolExec(ctx, []byte(toolJSON))
				messages = append(messages, Message{Role: "assistant", Content: toolJSON})
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

		// Last resort: if the output looks like a respond call but extractToolCall
		// failed (e.g. unescaped quotes in the text value), try regex extraction
		// to avoid sending raw JSON to the user.
		if text, ok := extractRespondFallback(stripped); ok {
			messages = append(messages, Message{Role: "assistant", Content: raw})
			return text, messages[historyLen:], nil
		}

		messages = append(messages, Message{Role: "assistant", Content: raw})
		return content, messages[historyLen:], nil
	}

	return "", messages[historyLen:], fmt.Errorf("too many tool call rounds")
}

// extractRespondText checks if a tool-call JSON is the "respond" meta-tool
// and returns the text content if so.
func extractRespondText(toolJSON string) (string, bool) {
	var tc struct {
		Name      string `json:"name"`
		Arguments struct {
			Text string `json:"text"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(toolJSON), &tc); err != nil {
		return "", false
	}
	if tc.Name == "respond" {
		return tc.Arguments.Text, true
	}
	return "", false
}

// respondFallbackRe matches a respond tool call with potentially malformed JSON.
// It captures everything between {"name":"respond","arguments":{"text":" and "}}
var respondFallbackRe = regexp.MustCompile(`(?s)\{\s*"name"\s*:\s*"respond"\s*,\s*"arguments"\s*:\s*\{\s*"text"\s*:\s*"(.+)"\s*\}\s*\}`)

// extractRespondFallback is a last-resort extractor for respond calls where the
// text value contains unescaped quotes that break JSON parsing. It uses a regex
// to pull out the text content.
func extractRespondFallback(s string) (string, bool) {
	m := respondFallbackRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return "", false
	}
	// Unescape any properly escaped sequences that survived.
	text := strings.ReplaceAll(m[1], `\"`, `"`)
	text = strings.ReplaceAll(text, `\\`, `\`)
	logger.Log.Warn("[llm] used respond fallback extractor (malformed JSON from LLM)")
	return text, true
}

// normalizeToolCallKeys checks for common alternative key names that LLMs use
// instead of "name" (e.g. "tool_name", "tool_call", "function") and remaps
// them. Returns the (possibly modified) map and whether a "name" key exists.
func normalizeToolCallKeys(obj map[string]any) bool {
	if _, ok := obj["name"]; ok {
	} else {
		found := false
		for _, alt := range []string{"tool_name", "tool_call", "function", "tool"} {
			if v, ok := obj[alt]; ok {
				obj["name"] = v
				delete(obj, alt)
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	// Normalize "args" → "arguments" if needed.
	if _, ok := obj["arguments"]; !ok {
		if v, ok := obj["args"]; ok {
			obj["arguments"] = v
			delete(obj, "args")
		}
	}
	return true
}

// extractToolCall extracts a JSON tool-call object from the start of s.
// It handles the common case where the LLM appends commentary after the JSON.
// Returns the extracted JSON string and true if a valid tool call was found.
func extractToolCall(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s[0] != '{' {
		return "", false
	}

	// Fast path: try unmarshaling the full string. This handles clean output
	// and avoids edge cases in the character walk (e.g. unescaped inner quotes).
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err == nil {
		if normalizeToolCallKeys(obj) {
			normalized, _ := json.Marshal(obj)
			return string(normalized), true
		}
	}

	// Slow path: walk the string to find the matching closing brace.
	// Needed when the LLM appends prose commentary after the JSON object.
	depth := 0
	inString := false
	escaped := false
	for i, c := range s {
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
				candidate := s[:i+1]
				if err := json.Unmarshal([]byte(candidate), &obj); err == nil {
					if normalizeToolCallKeys(obj) {
						normalized, _ := json.Marshal(obj)
						return string(normalized), true
					}
				}
				return "", false
			}
		}
	}
	return "", false
}
