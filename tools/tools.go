package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/logger"
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
type ToolSchema struct {
	Name   string
	Params []ParamSchema
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

// Register adds a tool to the registry with an optional schema for grammar generation.
func (r *Registry) Register(name string, fn ToolFunc, schema ...ToolSchema) {
	r.tools[name] = fn
	if len(schema) > 0 {
		r.schemas[name] = schema[0]
	}
	logger.Log.Infof("[tools] registered %s", name)
}

// ToolSchemas returns all registered tool schemas WITHOUT the built-in "respond"
// meta-tool. Used by the supervisor pattern where the tool agent should never
// call respond — the orchestrator responds in plain text instead.
func (r *Registry) ToolSchemas() []ToolSchema {
	var schemas []ToolSchema
	for _, s := range r.schemas {
		schemas = append(schemas, s)
	}
	return schemas
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
		preview := result
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		logger.Log.Infof("[tool] %s result: %s", tc.Name, preview)
	}
	return result, err
}
