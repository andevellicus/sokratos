package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/logger"
	"sokratos/textutil"
)

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
}

// Registry maps tool names to their implementations and optional schemas.
type Registry struct {
	tools   map[string]ToolFunc
	schemas map[string]ToolSchema
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

// CompactIndex returns a compact one-line-per-tool index for the system prompt.
// Required params are starred. The "respond" meta-tool is excluded (handled
// separately by the supervisor prompt).
func (r *Registry) CompactIndex() string {
	var b strings.Builder
	for _, s := range r.schemas {
		if s.Name == "respond" {
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

// DynamicToolDescriptions returns formatted descriptions for skill tools
// (IsSkill=true with a non-empty Description). These provide detailed argument
// info for runtime-registered skills that the orchestrator hasn't seen in
// training data. Appended after the compact tool index in the system prompt.
func (r *Registry) DynamicToolDescriptions() string {
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
	return b.String()
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
	if err != nil {
		logger.Log.Errorf("[tool] %s error: %v", tc.Name, err)
	} else {
		logger.Log.Infof("[tool] %s result: %s", tc.Name, textutil.Truncate(result, 200))
	}
	return result, err
}

// NewScopedToolExec returns a SubagentToolExec that validates tool availability
// against dc before delegating to the registry. Use this wherever a subagent
// supervisor loop needs restricted tool access.
func NewScopedToolExec(registry *Registry, dc *DelegateConfig) SubagentToolExec {
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
