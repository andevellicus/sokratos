package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"sokratos/httputil"
	"sokratos/prompts"
	"sokratos/timeouts"
)

// Client wraps communication with an OpenAI-compatible LLM server (e.g. llama-server).
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
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
	Model              string         `json:"model"`
	Messages           []Message      `json:"messages"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	Grammar            string         `json:"grammar,omitempty"`
	ReasoningFormat    string         `json:"reasoning_format,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
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
	Message      Message
	FinishReason string
}

// NewClient returns a Client configured to talk to the given LLM server base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: httputil.NewClient(timeouts.HTTPSafetyNet),
	}
}

// Chat sends a chat request and returns the assistant message.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
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
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		detail := strings.TrimSpace(string(errBody))
		if detail == "" {
			detail = "no response body"
		}
		return ChatResult{}, fmt.Errorf("llm server returned status %d: %s", resp.StatusCode, detail)
	}

	var raw chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return ChatResult{}, fmt.Errorf("decode response: %w", err)
	}

	if len(raw.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("llm server returned no choices")
	}

	return ChatResult{
		Message:      raw.Choices[0].Message,
		FinishReason: raw.Choices[0].FinishReason,
	}, nil
}


const maxToolRounds = 15
const defaultMaxToolResultLen = 8000 // truncate individual tool results to stay within context

// resolveToolResultLen returns the effective max tool result length, respecting
// the per-session override in opts when set.
func resolveToolResultLen(opts *QueryOrchestratorOpts) int {
	if opts != nil && opts.MaxToolResultLen > 0 {
		return opts.MaxToolResultLen
	}
	return defaultMaxToolResultLen
}

// FallbackDef describes a deterministic fallback tool to invoke when a primary
// tool fails. ArgsTransform builds new args from the original call context.
// TriggerPattern, when non-nil, restricts fallback to failures matching the
// pattern; nil means trigger on any failure.
type FallbackDef struct {
	FallbackTool   string
	ArgsTransform  func(toolName string, originalArgs json.RawMessage, failureMsg string) json.RawMessage
	TriggerPattern *regexp.Regexp // nil = trigger on any failure
}

// FallbackMap maps primary tool names to their fallback definitions.
type FallbackMap map[string]FallbackDef

var systemPromptBase = strings.TrimSpace(prompts.System)

// ToolAgentConfig holds the configuration for the supervisor pattern. When set,
// the orchestrator produces free-form text with <TOOL_INTENT> tags, and
// parseToolIntent translates intents into structured JSON.
type ToolAgentConfig struct {
	ToolDescriptions string          // full tool descriptions for system prompt (static + dynamic skills)
	Parser           ToolIntentParser // nil defaults to SupervisorParser{}
}

// BackgroundJobRequest is a sentinel error returned when the supervisor detects
// a tool call that should be handled by a background Brain session instead.
// Returned by both mandatory intercepts (create_skill, update_skill) and the
// deep_think tool when called with background=true.
type BackgroundJobRequest struct {
	Tool             string // triggering tool ("create_skill") or "deep_think"
	UserGoal         string // original user message (from supervisor's prompt param)
	TaskType         string // maps to session prompt; "" = general
	ProblemStatement string // for reason tool: the problem_statement arg
}

func (e *BackgroundJobRequest) Error() string {
	return "background job requested for " + e.Tool
}

// EscalationRequest is a sentinel error returned when the supervisor detects a
// tool call that requires a more capable model (e.g. Brain). The caller should
// release the current slot, acquire the target model, and replay the request.
type EscalationRequest struct {
	ToolName string // the tool that triggered escalation
}

func (e *EscalationRequest) Error() string {
	return "escalation requested for tool: " + e.ToolName
}

// QueryOrchestratorOpts holds optional parameters for QueryOrchestrator.
type QueryOrchestratorOpts struct {
	Parts              []ContentPart    // vision content parts for the user message
	History            []Message        // prior conversation history to prepend
	PersonalityContent string           // personality traits markdown — injected into system prompt before profile
	ProfileContent     string           // identity profile JSON — injected into system prompt if non-empty
	TemporalContext    string           // XML temporal context — injected after profile
	PrefetchContent    string           // retrieved memories XML — injected into system prompt at end
	MaxToolResultLen   int              // max chars per tool result (0 = default 2000)
	MaxWebSources      int              // replaces %MAX_WEB_SOURCES% in system prompt (0 = default 2)
	ToolAgent          *ToolAgentConfig // when set, enables the supervisor pattern
	Fallbacks          FallbackMap      // deterministic fallback chains for failed tools
	MandatedBrainTools map[string]string // tools that trigger a background Brain job (key=tool, value=task_type)
	EscalateTools      map[string]bool   // tools that trigger inline escalation to a more capable model
	OnToolStart    func(toolName string)                           // called before tool execution with tool name (nil = no-op)
	OnToolEnd      func(ctx context.Context) error                // reacquire slot after tool execution (nil = no-op)
	OnToolExec     func(toolName string, dur time.Duration, err error) // called after tool execution with timing (nil = no-op)
	EnableThinking bool // when true, enables chain-of-thought reasoning (for Brain background jobs)
}

// QueryOrchestrator sends a prompt to the given model, executing tool calls as
// needed, and returns the final human-readable response text along with the
// NEW messages produced during this call (excluding history). An optional
// trimFn is applied to the full message slice before each LLM call to keep the
// context window within limits.
func QueryOrchestrator(ctx context.Context, client *Client, model, prompt string, toolExec func(context.Context, json.RawMessage) (string, error), trimFn func([]Message) []Message, opts *QueryOrchestratorOpts) (string, []Message, error) {
	return querySupervisor(ctx, client, model, prompt, toolExec, trimFn, opts)
}
