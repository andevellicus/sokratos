package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/clients"
	"sokratos/prompts"
)

const maxDelegateContextLen = 12000

type delegateTaskArgs struct {
	Directive string `json:"directive"`
	Context   string `json:"context"`
}

var delegateSystemPrompt = strings.TrimSpace(prompts.DelegateSystem)

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

		result, err := clients.SubagentSupervisor(ctx, sc, dc.Grammar(), delegateSystemPrompt, directive, toolExec, 10, nil)
		if err != nil {
			return fmt.Sprintf("delegate_task failed: %v", err), nil
		}

		return result, nil
	}
}
