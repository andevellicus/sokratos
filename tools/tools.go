package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"sokratos/clients"
	"sokratos/google"
	"sokratos/logger"
	"sokratos/textutil"
)

// ToolError is a structured error for tool failures that should be forwarded
// to the LLM as a result string, not treated as a fatal internal error.
// Execute catches these via errors.As, logs at WARN, and converts to
// (message, nil) so the LLM sees the failure context and can respond.
type ToolError struct {
	Message string
}

func (e *ToolError) Error() string { return e.Message }

// Errorf constructs a ToolError with fmt.Sprintf-style formatting.
func Errorf(format string, args ...any) *ToolError {
	return &ToolError{Message: fmt.Sprintf(format, args...)}
}

// ParseArgs unmarshals JSON tool arguments into T, returning a consistent
// "invalid arguments" error on failure. Callers that use the soft-error
// convention can return (err.Error(), nil); callers that use ToolError can
// return ("", err).
func ParseArgs[T any](args json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(args, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

// ToolCall represents a tool invocation from the LLM.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolFunc is the signature every registered tool must satisfy.
type ToolFunc func(ctx context.Context, args json.RawMessage) (string, error)

// ParamSchema describes a single parameter for GBNF grammar generation.
type ParamSchema struct {
	Name     string
	Type     string // "string", "number", "boolean", "array"
	Required bool
}

// ToolSchema describes a tool's name and parameter types for grammar generation.
// Description is optional — when set, the tool is included in dynamic tool
// descriptions for the tool agent (used by skills and other runtime-registered tools).
type ToolSchema struct {
	Name        string
	Params      []ParamSchema
	Description string
	IsSkill     bool // true for user-created JS skills

	// ConfirmFormat builds a human-readable confirmation prompt for this tool.
	// nil → generic "Execute <name>?" fallback.
	ConfirmFormat func(args json.RawMessage) string
	// ConfirmCacheKey builds a cache key that identifies the specific action
	// being approved (e.g. recipient+subject for send_email). nil → tool name only.
	ConfirmCacheKey func(args json.RawMessage) string
}

// Registry maps tool names to their implementations and optional schemas.
type Registry struct {
	tools       map[string]ToolFunc
	schemas     map[string]ToolSchema
	OnAuthError func(toolName string) // called once when a ToolError contains an auth failure
}

// NewRegistry returns an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]ToolFunc),
		schemas: make(map[string]ToolSchema),
	}
}

// Has returns true if a tool with the given name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok
}

// Unregister removes a tool from the registry.
func (r *Registry) Unregister(name string) {
	delete(r.tools, name)
	delete(r.schemas, name)
	logger.Log.Infof("[tools] unregistered %s", name)
}

// Register adds a tool to the registry with an optional schema for grammar generation.
func (r *Registry) Register(name string, fn ToolFunc, schema ...ToolSchema) {
	r.tools[name] = fn
	if len(schema) > 0 {
		r.schemas[name] = schema[0]
	}
	logger.Log.Infof("[tools] registered %s", name)
}

// Schemas returns all registered tool schemas plus the built-in "respond" schema.
func (r *Registry) Schemas() []ToolSchema {
	var schemas []ToolSchema
	for _, s := range r.schemas {
		schemas = append(schemas, s)
	}
	// Built-in respond meta-tool.
	schemas = append(schemas, ToolSchema{
		Name: "respond",
		Params: []ParamSchema{
			{Name: "text", Type: "string", Required: true},
		},
	})
	return schemas
}

// SchemasForTools returns schemas for the named subset of tools. Tools not
// found in the registry are silently skipped.
func (r *Registry) SchemasForTools(names []string) []ToolSchema {
	var schemas []ToolSchema
	for _, name := range names {
		if s, ok := r.schemas[name]; ok {
			schemas = append(schemas, s)
		}
	}
	return schemas
}

// SchemaFor returns the schema for a named tool, if registered.
func (r *Registry) SchemaFor(name string) (ToolSchema, bool) {
	s, ok := r.schemas[name]
	return s, ok
}

// CompactIndex returns a compact one-line-per-tool index for the system prompt.
// Required params are starred. Skills (IsSkill=true) and the "respond" meta-tool
// are excluded — skills get their own section via DynamicSkillDescriptions.
func (r *Registry) CompactIndex() string {
	var b strings.Builder
	for _, s := range r.schemas {
		if s.Name == "respond" || s.IsSkill {
			continue
		}
		b.WriteString("- ")
		b.WriteString(s.Name)
		if len(s.Params) > 0 {
			b.WriteString("(")
			for i, p := range s.Params {
				if i > 0 {
					b.WriteString(", ")
				}
				if p.Required {
					b.WriteString("*")
				}
				b.WriteString(p.Name)
			}
			b.WriteString(")")
		}
		if s.Description != "" {
			b.WriteString(" — ")
			b.WriteString(s.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// DynamicSkillDescriptions returns a formatted "## Skills" section for
// runtime-registered skills (IsSkill=true). Separate from the tool index so the
// orchestrator sees skills as a distinct concept.
func (r *Registry) DynamicSkillDescriptions() string {
	var b strings.Builder
	for _, s := range r.schemas {
		if !s.IsSkill || s.Description == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(s.Name)
		b.WriteString(": ")
		b.WriteString(s.Description)
		if len(s.Params) > 0 {
			b.WriteString(" Arguments: {")
			for i, p := range s.Params {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "\"%s\": \"<%s>\"", p.Name, p.Type)
			}
			b.WriteString("}")
		} else {
			b.WriteString(" No arguments.")
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "## Skills\n\n" + b.String()
}

// Call is a convenience method that marshals args into JSON and invokes the
// named tool. Use this when calling a tool programmatically without constructing
// a raw ToolCall JSON payload.
func (r *Registry) Call(ctx context.Context, name string, args any) (string, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshal args for %s: %w", name, err)
	}
	raw, err := json.Marshal(ToolCall{Name: name, Arguments: argsJSON})
	if err != nil {
		return "", fmt.Errorf("marshal tool call for %s: %w", name, err)
	}
	return r.Execute(ctx, raw)
}

// looksLikeError is a transitional fallback for tools that still return
// errors as (errorString, nil) instead of using ToolError. Prefer returning
// a *ToolError from new/updated tools.
func looksLikeError(result string) bool {
	lower := strings.ToLower(result)
	return strings.HasPrefix(lower, "failed to") ||
		strings.HasPrefix(lower, "error:") ||
		strings.HasPrefix(lower, "invalid") ||
		strings.HasPrefix(result, "⚠")
}

// Execute parses raw JSON into a ToolCall, looks up the tool, and invokes it.
func (r *Registry) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var tc ToolCall
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "", fmt.Errorf("invalid tool call JSON: %w", err)
	}
	fn, ok := r.tools[tc.Name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", tc.Name)
	}

	logger.Log.Infof("[tool] calling %s(%s)", tc.Name, strings.TrimSpace(string(tc.Arguments)))
	result, err := fn(ctx, tc.Arguments)

	// Structured tool errors: catch ToolError, log at WARN, forward message
	// to the LLM as a normal string result.
	var te *ToolError
	if errors.As(err, &te) {
		logger.Log.Warnf("[tool] %s failed: %s", tc.Name, te.Message)
		// Fire auth error callback for proactive notification.
		if r.OnAuthError != nil && strings.Contains(te.Message, "authorization") {
			r.OnAuthError(tc.Name)
		}
		return te.Message, nil
	}

	// Centralized Google auth error detection. If any tool's underlying
	// API call fails due to an expired/revoked token, convert it to a ToolError
	// and fire the callback so the orchestrator knows to stop trying and notify the user.
	if err != nil && google.IsAuthError(err) {
		logger.Log.Warnf("[tool] %s failed with auth error: %v", tc.Name, err)
		if r.OnAuthError != nil {
			r.OnAuthError(tc.Name)
		}
		return google.AuthErrorMessage, nil
	}

	if err != nil {
		logger.Log.Errorf("[tool] %s error: %v", tc.Name, err)
	} else if looksLikeError(result) {
		// Fallback for tools still returning errors as (string, nil).
		logger.Log.Warnf("[tool] %s result: %s", tc.Name, textutil.Truncate(result, 200))
	} else {
		logger.Log.Infof("[tool] %s result: %s", tc.Name, textutil.Truncate(result, 200))
	}
	return result, err
}

// NewScopedToolExec returns a SubagentToolExec that validates tool availability
// against dc before delegating to the registry. Use this wherever a subagent
// supervisor loop needs restricted tool access.
func NewScopedToolExec(registry *Registry, dc *DelegateConfig) clients.SubagentToolExec {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var tc ToolCall
		if err := json.Unmarshal(raw, &tc); err != nil {
			return "", fmt.Errorf("invalid tool call JSON: %w", err)
		}
		if !dc.IsAllowed(tc.Name) {
			return fmt.Sprintf("tool %q is not available", tc.Name), nil
		}
		return registry.Execute(ctx, raw)
	}
}
