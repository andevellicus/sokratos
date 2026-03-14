// Package orchestrate provides a unified grammar-constrained LLM tool-dispatch
// loop used by both the main orchestrator (llm/) and the subagent supervisor
// (clients/). The LLM backend is abstracted behind ChatFunc, avoiding import
// cycles between llm/ and clients/.
package orchestrate

import (
	"context"
	"encoding/json"
	"regexp"
	"time"
)

// Message is a minimal chat message used within the loop. Callers convert
// to/from their package-specific message types at the boundary.
type Message struct {
	Role    string
	Content string
}

// ChatInput holds everything the loop needs to make a single LLM call.
type ChatInput struct {
	Messages           []Message
	Grammar            string
	MaxTokens          int
	ReasoningFormat    string         // "none" or "deepseek"
	ChatTemplateKwargs map[string]any // e.g. {"enable_thinking": true}
}

// ChatFunc abstracts the LLM call. Both llm.Client.Chat and
// SubagentClient.CompleteMultiTurnWithGrammar are wrapped into this form.
type ChatFunc func(ctx context.Context, req ChatInput) (string, error)

// ToolExecFunc executes a tool call given its raw JSON
// ({"name":"...","arguments":{...}}). Returns the tool result string or error.
type ToolExecFunc func(ctx context.Context, raw json.RawMessage) (string, error)

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

// BackgroundJobRequest is a sentinel error returned when the orchestrator
// detects a tool call that should be handled by a background Brain session.
// Returned by both mandatory intercepts (create_skill, update_skill) and the
// deep_think tool when called with background=true.
type BackgroundJobRequest struct {
	Tool             string // triggering tool ("create_skill") or "deep_think"
	UserGoal         string // original user message
	TaskType         string // maps to session prompt; "" = general
	ProblemStatement string // for deep_think: the problem_statement arg
}

func (e *BackgroundJobRequest) Error() string {
	return "background job requested for " + e.Tool
}

// LoopConfig controls which features are active for a RunLoop invocation.
type LoopConfig struct {
	// Grammar is the GBNF grammar constraining output to tool/respond JSON.
	Grammar   string
	MaxTokens int // per-round LLM max tokens (0 = default 4096)

	// MaxRounds caps tool-call rounds (0 = default 15).
	MaxRounds int
	// MaxToolResultLen truncates individual tool results (0 = default 8000).
	MaxToolResultLen int
	// ToolResultOverflowHint appended when tool results are truncated (empty = default).
	ToolResultOverflowHint string

	// Thinking
	EnableThinking     bool // all rounds
	FirstRoundThinking bool // round 0 only

	// Callbacks (all nil-safe)
	OnToolStart func(toolName string)
	OnToolEnd   func(ctx context.Context) error
	OnToolExec  func(toolName string, dur time.Duration, err error)
	ProgressFn  func(string)

	// Advanced features (off by default for subagent use)
	Fallbacks          FallbackMap
	MandatedBrainTools map[string]string // tool -> task_type; intercept before execution
	InjectTimestamp    bool              // inject "Current time: ..." into system prompt each round
	SoftErrorDetection bool             // detect soft errors in tool results

	// TrimFn is called before each LLM call to keep messages within context.
	TrimFn func([]Message) []Message

	// UserGoal is the original user message, used for BackgroundJobRequest.
	UserGoal string
}
