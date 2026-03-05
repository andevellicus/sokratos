package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"sokratos/clients"
)

const maxDelegateContextLen = 12000

type delegateTaskArgs struct {
	Directive string `json:"directive"`
	Context   string `json:"context"`
}

const delegateSystemPrompt = `You are a research assistant with access to tools for searching, reading, and saving information.

## Rules

- Execute the user's directive by calling the available tools as needed.
- For research tasks, use multi-hop strategies: search first, then read promising URLs, then synthesize.
- When you have gathered enough information, respond with your findings.
- Use save_memory to persist important findings that should be remembered long-term.
- Be concise and factual. No preamble or explanation of your process.
- If a tool returns an error, try an alternative approach before giving up.
- If you cannot complete the task, explain why in your response.`

// DelegateConfig holds the mutable grammar and allowed-tools list for
// delegate_task. Updated atomically when skills are created or deleted.
type DelegateConfig struct {
	mu      sync.RWMutex
	grammar string
	allowed map[string]bool
}

// NewDelegateConfig creates a config with the given tool names and grammar.
func NewDelegateConfig(tools []string, grammar string) *DelegateConfig {
	allowed := make(map[string]bool, len(tools))
	for _, name := range tools {
		allowed[name] = true
	}
	return &DelegateConfig{grammar: grammar, allowed: allowed}
}

// Update replaces the grammar and allowed-tools list atomically.
func (dc *DelegateConfig) Update(tools []string, grammar string) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.grammar = grammar
	dc.allowed = make(map[string]bool, len(tools))
	for _, name := range tools {
		dc.allowed[name] = true
	}
}

// Grammar returns the current GBNF grammar.
func (dc *DelegateConfig) Grammar() string {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.grammar
}

// IsAllowed returns whether the named tool is delegatable.
func (dc *DelegateConfig) IsAllowed(name string) bool {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.allowed[name]
}

// NewDelegateTask returns a ToolFunc that delegates a task to a subagent with
// access to a configurable set of tools via a lightweight multi-turn supervisor
// loop. The grammar and allowed-tools list are read from dc on each invocation
// so that newly created skills are immediately available.
func NewDelegateTask(sc *clients.SubagentClient, registry *Registry, dc *DelegateConfig) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		a, err := ParseArgs[delegateTaskArgs](args)
		if err != nil {
			return err.Error(), nil
		}
		if strings.TrimSpace(a.Directive) == "" {
			return "directive is required", nil
		}

		directive := a.Directive
		if a.Context != "" {
			ctxData := a.Context
			if len(ctxData) > maxDelegateContextLen {
				ctxData = ctxData[:maxDelegateContextLen] + "\n... (truncated)"
			}
			directive = fmt.Sprintf("%s\n\n## Context\n%s", a.Directive, ctxData)
		}

		toolExec := NewScopedToolExec(registry, dc)

		result, err := clients.SubagentSupervisor(ctx, sc, dc.Grammar(), delegateSystemPrompt, directive, toolExec, 10)
		if err != nil {
			return fmt.Sprintf("delegate_task failed: %v", err), nil
		}

		return result, nil
	}
}
