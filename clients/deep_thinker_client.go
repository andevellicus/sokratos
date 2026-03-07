package clients

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/engine"
	"sokratos/httputil"
	"sokratos/memory"
	"sokratos/timeouts"
)

// DeepThinkerClient owns the shared HTTP client, URL, and model for all
// deep-thinker interactions (triage, consolidation, consult). A semaphore
// limits concurrency to 1, matching the DTC server's single 32K slot.
type DeepThinkerClient struct {
	baseClient
	sem *engine.PrioritySem
	wq  *WorkQueue
}

// NewDeepThinkerClient returns a ready-to-use client with the HTTP safety-net
// timeout and a concurrency semaphore of 1 (Qwen3.5-27B runs --parallel 1 with
// full 32K context for heavy reasoning; subagent overflow queues behind DTC).
// Per-call timeouts are controlled via context deadlines, not the HTTP client.
func NewDeepThinkerClient(url, model string) *DeepThinkerClient {
	d := &DeepThinkerClient{
		baseClient: baseClient{
			URL:    url,
			Model:  model,
			client: httputil.NewClient(timeouts.HTTPSafetyNet),
			cb:     newCircuitBreaker("dtc"),
			logTag: "[dtc]",
		},
		sem: engine.NewPrioritySem(1),
	}
	d.wq = NewWorkQueue(16, 1, d.logTag, func(ctx context.Context, req memory.WorkRequest) (string, error) {
		if req.Grammar != "" {
			return d.CompleteNoThinkWithGrammar(ctx, req.SystemPrompt, req.UserPrompt, req.Grammar, req.MaxTokens)
		}
		return d.CompleteNoThink(ctx, req.SystemPrompt, req.UserPrompt, req.MaxTokens)
	})
	return d
}

// QueueWork submits a background task to the shared work queue.
func (d *DeepThinkerClient) QueueWork(item memory.WorkRequest) {
	d.wq.QueueWork(item)
}

// TryAcquire attempts to acquire the Brain's single slot non-blockingly.
// Returns true if acquired (caller MUST call Release). Used by the slot
// router to check if the Brain is available for orchestrator work.
func (d *DeepThinkerClient) TryAcquire() bool {
	return d.sem.TryAcquire()
}

// TryAcquireAt attempts a priority-aware non-blocking acquire.
// Exported for the slot router (SlotChecker interface).
func (d *DeepThinkerClient) TryAcquireAt(pri engine.Priority) bool {
	return d.sem.TryAcquireAt(pri)
}

// Acquire blocks until the Brain's slot is available or ctx is cancelled.
func (d *DeepThinkerClient) Acquire(ctx context.Context, pri engine.Priority) error {
	return d.sem.Acquire(ctx, pri)
}

// Release frees the Brain's slot. Must be called after TryAcquire/Acquire succeeds.
func (d *DeepThinkerClient) Release() {
	d.sem.Release()
}

// ReleaseReserved frees a slot with a reservation for high-priority reacquire.
// Exported for the slot router (SlotChecker interface).
func (d *DeepThinkerClient) ReleaseReserved() {
	d.sem.ReleaseReserved()
}

// CancelReservation cancels an outstanding reservation.
// Exported for the slot router (SlotChecker interface).
func (d *DeepThinkerClient) CancelReservation() {
	d.sem.CancelReservation()
}

// Complete sends a system+user message pair to the deep thinker with thinking
// enabled. Qwen3.5-27B's Jinja template uses chat_template_kwargs to control
// thinking; reasoning_format "deepseek" makes llama-server split <think> blocks
// into reasoning_content, keeping the content field clean.
func (d *DeepThinkerClient) Complete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return d.complete(ctx, systemPrompt, userContent, maxTokens, true, "")
}

// CompleteNoThink is like Complete but disables chain-of-thought reasoning via
// chat_template_kwargs {"enable_thinking": false}. Qwen3.5-27B produces clean
// output with zero reasoning tokens, making this the preferred path for
// structured output tasks (consolidation JSON, plan decomposition, etc.).
func (d *DeepThinkerClient) CompleteNoThink(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return d.complete(ctx, systemPrompt, userContent, maxTokens, false, "")
}

// CompleteNoThinkWithGrammar is like CompleteNoThink but adds a GBNF grammar
// constraint to guarantee structured output (e.g. triage JSON). Thinking is
// disabled so all tokens go toward the constrained output.
func (d *DeepThinkerClient) CompleteNoThinkWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error) {
	return d.complete(ctx, systemPrompt, userContent, maxTokens, false, grammar)
}

// complete is the internal implementation shared by Complete, CompleteNoThink,
// and CompleteNoThinkWithGrammar. enableThinking controls Qwen3.5's Jinja
// template via chat_template_kwargs. When thinking is enabled, reasoning_format
// "deepseek" separates <think> output from content. When thinking is disabled,
// reasoning_format "none" overrides llama-server's auto-detection to prevent
// interference with grammar constraints.
func (d *DeepThinkerClient) complete(ctx context.Context, systemPrompt, userContent string, maxTokens int, enableThinking bool, grammar string) (string, error) {
	if err := d.cb.check(); err != nil {
		return "", err
	}

	// Acquire the DTC slot before hitting the server.
	if err := d.sem.Acquire(ctx, engine.PriorityBackground); err != nil {
		return "", err
	}
	defer d.sem.Release()

	if err := d.ensureLoaded(ctx); err != nil {
		d.cb.recordFailureIfServer(err)
		return "", fmt.Errorf("model not available: %w", err)
	}

	reasoningFmt := "none"
	if enableThinking {
		reasoningFmt = "deepseek"
	}

	payload := chatRequest{
		Model: d.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature:     0.1,
		MaxTokens:       maxTokens,
		Grammar:         grammar,
		ReasoningFormat: reasoningFmt,
		ChatTemplateKwargs: map[string]any{
			"enable_thinking": enableThinking,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	result, err := d.doRequest(ctx, body)
	if err != nil {
		d.cb.recordFailureIfServer(err)
		return "", err
	}

	d.cb.recordSuccess()
	return result, nil
}
