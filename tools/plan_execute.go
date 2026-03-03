package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"sokratos/clients"
	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/textutil"
)

// scratchpadBudget is the max characters per scratchpad entry.
const scratchpadBudget = 1500

// complexKeywords triggers DTC routing when found in step descriptions.
var complexKeywords = []string{
	"analyze", "synthesize", "compare", "evaluate",
	"summarize across", "cross-reference", "identify patterns", "consolidate",
}

// retrievalTools are tools that indicate a simple retrieval step.
var retrievalTools = map[string]bool{
	"search_email":    true,
	"search_calendar": true,
	"search_memory":   true,
	"search_web":      true,
	"read_url":        true,
}

// Scratchpad provides structured key-value context between plan steps.
// Each entry is truncated to scratchpadBudget characters to prevent context
// inflation across multi-step plans.
type Scratchpad struct {
	mu      sync.RWMutex
	entries []scratchpadEntry // ordered slice preserves insertion order
}

type scratchpadEntry struct {
	Key   string
	Value string
}

// Set stores a value in the scratchpad, truncated to scratchpadBudget.
func (s *Scratchpad) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	truncated := textutil.Truncate(value, scratchpadBudget)
	for i := range s.entries {
		if s.entries[i].Key == key {
			s.entries[i].Value = truncated
			return
		}
	}
	s.entries = append(s.entries, scratchpadEntry{Key: key, Value: truncated})
}

// Get retrieves a value from the scratchpad. Returns "" if not found.
func (s *Scratchpad) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.Key == key {
			return e.Value
		}
	}
	return ""
}

// FormatForPrompt renders the scratchpad as a bullet list for system prompt injection.
func (s *Scratchpad) FormatForPrompt() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.entries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range s.entries {
		fmt.Fprintf(&b, "- %s: %s\n", e.Key, e.Value)
	}
	return b.String()
}

// planAndExecuteArgs are the arguments parsed from the orchestrator's tool call.
type planAndExecuteArgs struct {
	Directive  string `json:"directive"`
	Context    string `json:"context"`
	Background bool   `json:"background"`
	Priority   int    `json:"priority"`
}

// planStep represents a single decomposed step from DTC.
type planStep struct {
	Description string   `json:"description"`
	ToolsNeeded []string `json:"tools_needed"`
}

// taskPlan is the structured output from DTC decomposition.
type taskPlan struct {
	Steps []planStep `json:"steps"`
}

// stepResult records the outcome of executing a single step.
type stepResult struct {
	Step        int
	Description string
	Result      string
	Success     bool
}

// decomposePlan calls DTC to break a directive into concrete steps.
func decomposePlan(ctx context.Context, dtc *clients.DeepThinkerClient, directive, extraContext string) (*taskPlan, error) {
	userContent := directive
	if extraContext != "" {
		userContent = fmt.Sprintf("%s\n\nContext:\n%s", directive, extraContext)
	}

	decompCtx, cancel := context.WithTimeout(ctx, TimeoutPlanDecomposition)
	defer cancel()

	raw, err := dtc.CompleteNoThink(decompCtx, strings.TrimSpace(prompts.PlanTask), userContent, 2048)
	if err != nil {
		return nil, fmt.Errorf("plan decomposition: %w", err)
	}

	raw = textutil.CleanLLMJSON(raw)

	var plan taskPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return nil, fmt.Errorf("parse plan: %w (raw: %s)", err, raw)
	}

	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("plan produced zero steps")
	}
	const maxPlanSteps = 6
	if len(plan.Steps) > maxPlanSteps {
		plan.Steps = plan.Steps[:maxPlanSteps]
		logger.Log.Warnf("[plan] truncated plan to %d steps", maxPlanSteps)
	}

	return &plan, nil
}

// executeSteps runs each plan step sequentially. Simple steps go through
// SubagentSupervisor (Flash); complex synthesis/analysis steps route to DTC
// directly. A scratchpad carries concise context between steps. When dtc is
// non-nil, mid-flight replanning is enabled on step failures.
func executeSteps(ctx context.Context, sc *clients.SubagentClient, dtc *clients.DeepThinkerClient,
	dc *DelegateConfig, registry *Registry, directive string, steps []planStep,
	progressFn func(completed, total int)) []stepResult {

	results := make([]stepResult, 0, len(steps))
	pad := &Scratchpad{}
	replanned := false
	consecutiveFailures := 0

	for i := 0; i < len(steps); i++ {
		step := steps[i]

		select {
		case <-ctx.Done():
			results = append(results, stepResult{
				Step:        i + 1,
				Description: step.Description,
				Result:      "cancelled: " + ctx.Err().Error(),
				Success:     false,
			})
			return results
		default:
		}

		systemPrompt := buildStepSystemPrompt(directive, step, pad)
		stepCtx, stepCancel := context.WithTimeout(ctx, TimeoutPlanStepExecution)

		var result string
		var err error

		if isComplexStep(step) && dtc != nil {
			// Route complex reasoning steps to DTC directly (no tool access needed).
			logger.Log.Infof("[plan] step %d/%d routed to DTC (complex): %s", i+1, len(steps), step.Description)
			result, err = dtc.Complete(stepCtx, systemPrompt, step.Description, 4096)
		} else {
			// Simple retrieval steps go through Flash via SubagentSupervisor.
			logger.Log.Infof("[plan] executing step %d/%d: %s", i+1, len(steps), step.Description)
			toolExec := NewScopedToolExec(registry, dc)
			result, err = clients.SubagentSupervisor(stepCtx, sc, dc.Grammar(), systemPrompt,
				step.Description, toolExec, 10)
		}
		stepCancel()

		sr := stepResult{
			Step:        i + 1,
			Description: step.Description,
			Success:     err == nil,
		}
		if err != nil {
			sr.Result = fmt.Sprintf("step failed: %v", err)
			logger.Log.Warnf("[plan] step %d failed: %v", i+1, err)
			consecutiveFailures++

			// Record failure in scratchpad.
			existing := pad.Get("failures")
			line := fmt.Sprintf("Step %d failed: %v", i+1, err)
			if existing != "" {
				pad.Set("failures", existing+"; "+line)
			} else {
				pad.Set("failures", line)
			}
		} else {
			sr.Result = result
			logger.Log.Infof("[plan] step %d completed", i+1)
			consecutiveFailures = 0
		}

		// Store result in scratchpad.
		pad.Set(fmt.Sprintf("step_%d", i+1), sr.Result)
		results = append(results, sr)

		if progressFn != nil {
			progressFn(i+1, len(steps))
		}

		// Check replan triggers: step failed + more than 1 step remaining + not already replanned.
		remaining := len(steps) - (i + 1)
		shouldReplan := !replanned && dtc != nil && remaining > 0 && !sr.Success &&
			(consecutiveFailures >= 2 || remaining <= 2)
		if shouldReplan {
			newSteps, replanErr := replanRemaining(ctx, dtc, directive, pad, steps[i+1:])
			if replanErr != nil {
				logger.Log.Warnf("[plan] replanning failed: %v", replanErr)
			} else {
				logger.Log.Infof("[plan] replanning after step %d failure: %d remaining steps replaced", i+1, len(newSteps))
				// Replace the remaining steps with the new plan.
				steps = append(steps[:i+1], newSteps...)
				replanned = true
			}
		}
	}

	return results
}

// buildStepSystemPrompt constructs the system prompt for a single step,
// using the scratchpad for concise accumulated context instead of raw results.
func buildStepSystemPrompt(directive string, step planStep, pad *Scratchpad) string {
	var b strings.Builder
	b.WriteString("You are executing one step of a multi-step plan.\n\n")
	fmt.Fprintf(&b, "## Overall Goal\n%s\n\n", directive)
	fmt.Fprintf(&b, "## Your Current Step\n%s\n\n", step.Description)

	if context := pad.FormatForPrompt(); context != "" {
		b.WriteString("## Context from Prior Steps\n")
		b.WriteString(context)
		b.WriteByte('\n')
	}

	b.WriteString("## Rules\n")
	b.WriteString("- Execute your assigned step using the available tools.\n")
	b.WriteString("- Build upon context from prior steps when relevant.\n")
	b.WriteString("- Be concise and factual in your response.\n")
	b.WriteString("- When you have completed the step, respond with your findings.\n")

	return b.String()
}

// isComplexStep classifies a step as complex (requiring DTC) or simple (Flash).
// Steps with only retrieval tools are always simple. Otherwise, keyword
// heuristics on the description determine complexity.
func isComplexStep(step planStep) bool {
	// If tools_needed contains only retrieval tools, always simple.
	if len(step.ToolsNeeded) > 0 {
		allRetrieval := true
		for _, t := range step.ToolsNeeded {
			if !retrievalTools[t] {
				allRetrieval = false
				break
			}
		}
		if allRetrieval {
			return false
		}
	}

	desc := strings.ToLower(step.Description)
	for _, kw := range complexKeywords {
		if strings.Contains(desc, kw) {
			return true
		}
	}
	return false
}

// replanRemaining asks DTC to produce a revised plan for the remaining steps
// given the current scratchpad state and failures.
func replanRemaining(ctx context.Context, dtc *clients.DeepThinkerClient, directive string,
	pad *Scratchpad, remainingSteps []planStep) ([]planStep, error) {

	var b strings.Builder
	fmt.Fprintf(&b, "Original goal: %s\n\n", directive)
	b.WriteString("Completed context:\n")
	b.WriteString(pad.FormatForPrompt())
	b.WriteString("\nRemaining steps that need revision:\n")
	for i, s := range remainingSteps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s.Description)
	}

	replanCtx, cancel := context.WithTimeout(ctx, TimeoutPlanDecomposition)
	defer cancel()

	raw, err := dtc.CompleteNoThink(replanCtx, strings.TrimSpace(prompts.ReplanTask), b.String(), 2048)
	if err != nil {
		return nil, fmt.Errorf("replan call: %w", err)
	}

	raw = textutil.CleanLLMJSON(raw)

	var plan taskPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return nil, fmt.Errorf("parse replan: %w (raw: %s)", err, raw)
	}

	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("replan produced zero steps")
	}
	const maxReplanSteps = 4
	if len(plan.Steps) > maxReplanSteps {
		plan.Steps = plan.Steps[:maxReplanSteps]
	}

	return plan.Steps, nil
}

// formatResults formats step results into a human-readable summary.
func formatResults(results []stepResult) string {
	var b strings.Builder
	succeeded := 0
	for _, r := range results {
		if r.Success {
			succeeded++
		}
	}
	fmt.Fprintf(&b, "Plan completed: %d/%d steps succeeded.\n\n", succeeded, len(results))

	for _, r := range results {
		status := "OK"
		if !r.Success {
			status = "FAILED"
		}
		fmt.Fprintf(&b, "**Step %d** [%s]: %s\n%s\n\n", r.Step, status, r.Description, r.Result)
	}
	return b.String()
}

// NewPlanAndExecute returns a ToolFunc that decomposes a directive into steps
// via DTC, then executes them via SubagentSupervisor with accumulated context.
// When background=true, planning runs synchronously but execution is async.
func NewPlanAndExecute(dtc *clients.DeepThinkerClient, sc *clients.SubagentClient,
	dc *DelegateConfig, registry *Registry, wt *WorkTracker) ToolFunc {

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a planAndExecuteArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if strings.TrimSpace(a.Directive) == "" {
			return "directive is required", nil
		}
		if a.Priority < 1 || a.Priority > 10 {
			a.Priority = 5
		}

		extraContext := a.Context
		if len(extraContext) > maxDelegateContextLen {
			extraContext = extraContext[:maxDelegateContextLen] + "\n... (truncated)"
		}

		// Phase 1: Decompose (always synchronous).
		plan, err := decomposePlan(ctx, dtc, a.Directive, extraContext)
		if err != nil {
			return fmt.Sprintf("Failed to decompose plan: %v", err), nil
		}

		logger.Log.Infof("[plan] decomposed into %d steps for: %s", len(plan.Steps), a.Directive)
		for i, s := range plan.Steps {
			logger.Log.Infof("[plan]   step %d: %s (tools: %v)", i+1, s.Description, s.ToolsNeeded)
		}

		// Phase 2: Execute.
		if a.Background && wt != nil {
			taskID, err := wt.Start(a.Directive, a.Priority, plan.Steps, sc, dtc, dc, registry)
			if err != nil {
				return fmt.Sprintf("Failed to start background task: %v", err), nil
			}
			return fmt.Sprintf("Background task #%d started with %d steps. Use check_background_task to monitor progress.", taskID, len(plan.Steps)), nil
		}

		// Foreground mode.
		fgCtx, cancel := context.WithTimeout(ctx, TimeoutPlanForeground)
		defer cancel()

		results := executeSteps(fgCtx, sc, dtc, dc, registry, a.Directive, plan.Steps, nil)
		return formatResults(results), nil
	}
}
