package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/httputil"
)

// DeepThinkerClient owns the shared HTTP client, URL, and model for all
// deep-thinker interactions (triage, consolidation, consult). A semaphore
// limits concurrency to 1, matching the DTC server's single 32K slot.
type DeepThinkerClient struct {
	baseClient
	sem chan struct{}
}

// NewDeepThinkerClient returns a ready-to-use client with the HTTP safety-net
// timeout and a concurrency semaphore of 1 (Qwen3.5-27B runs --parallel 1 with
// full 32K context for heavy reasoning; subagent overflow queues behind DTC).
// Per-call timeouts are controlled via context deadlines, not the HTTP client.
func NewDeepThinkerClient(url, model string) *DeepThinkerClient {
	return &DeepThinkerClient{
		baseClient: baseClient{
			URL:    url,
			Model:  model,
			client: httputil.NewClient(TimeoutHTTPSafetyNet),
			cb:     newCircuitBreaker("dtc"),
			logTag: "[dtc]",
		},
		sem: make(chan struct{}, 1),
	}
}

// Complete sends a system+user message pair to the deep thinker with thinking
// enabled. Qwen3.5-27B's Jinja template uses chat_template_kwargs to control
// thinking; reasoning_format "deepseek" makes llama-server split <think> blocks
// into reasoning_content, keeping the content field clean.
func (d *DeepThinkerClient) Complete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return d.complete(ctx, systemPrompt, userContent, maxTokens, true)
}

// CompleteNoThink is like Complete but disables chain-of-thought reasoning via
// chat_template_kwargs {"enable_thinking": false}. Qwen3.5-27B produces clean
// output with zero reasoning tokens, making this the preferred path for
// structured output tasks (consolidation JSON, plan decomposition, etc.).
func (d *DeepThinkerClient) CompleteNoThink(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return d.complete(ctx, systemPrompt, userContent, maxTokens, false)
}

// complete is the internal implementation shared by Complete and CompleteNoThink.
// enableThinking controls Qwen3.5's Jinja template via chat_template_kwargs.
// reasoning_format "deepseek" is always set so llama-server correctly separates
// any <think> output from the content field.
func (d *DeepThinkerClient) complete(ctx context.Context, systemPrompt, userContent string, maxTokens int, enableThinking bool) (string, error) {
	if err := d.cb.check(); err != nil {
		return "", err
	}

	// Acquire the DTC slot before hitting the server.
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-d.sem }()

	if err := d.ensureLoaded(ctx); err != nil {
		d.cb.recordFailureIfServer(err)
		return "", fmt.Errorf("model not available: %w", err)
	}

	payload := chatRequest{
		Model: d.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature:     0.1,
		MaxTokens:       maxTokens,
		ReasoningFormat: "deepseek",
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

