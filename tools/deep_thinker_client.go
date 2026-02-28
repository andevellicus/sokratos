package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/httputil"
)

// DeepThinkerClient owns the shared HTTP client, URL, and model for all
// deep-thinker interactions (triage, consolidation, consult). A semaphore
// limits concurrency to 1, matching Z1's single 32K slot.
type DeepThinkerClient struct {
	baseClient
	sem chan struct{}
}

// NewDeepThinkerClient returns a ready-to-use client with a shared 120s HTTP
// transport and a concurrency semaphore of 1 (Z1 runs --parallel 1 with full
// 32K context for heavy reasoning; subagent overflow queues behind DTC).
func NewDeepThinkerClient(url, model string) *DeepThinkerClient {
	return &DeepThinkerClient{
		baseClient: baseClient{
			URL:    url,
			Model:  model,
			client: httputil.NewClient(TimeoutDeepThinker),
			cb:     newCircuitBreaker("dtc"),
			logTag: "[dtc]",
		},
		sem: make(chan struct{}, 1),
	}
}

// triageResult is the structured output from a triage call.
type triageResult struct {
	SalienceScore float64  `json:"salience_score"`
	Summary       string   `json:"summary"`
	Category      string   `json:"category"`
	Tags          []string `json:"tags"`
	Save          *bool    `json:"save,omitempty"`
	ParadigmShift bool     `json:"paradigm_shift,omitempty"`
}

// Complete sends a system+user message pair to the deep thinker and returns the
// raw content string. It is the low-level building block for ConsultDeepThinker
// and episode/reflection synthesis. Thinking is enabled (default).
func (d *DeepThinkerClient) Complete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return d.complete(ctx, systemPrompt, userContent, maxTokens, nil)
}

// CompleteNoThink is like Complete but explicitly disables chain-of-thought
// reasoning. WARNING: GLM-Z1 ignores think:false and still produces reasoning,
// but llama-server routes everything to reasoning_content (leaving content
// empty). The doRequest fallback then returns the full reasoning+output mixed
// together, breaking JSON extraction. Prefer Complete for Z1 — it properly
// separates reasoning from content.
func (d *DeepThinkerClient) CompleteNoThink(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return d.complete(ctx, systemPrompt, userContent, maxTokens, thinkFalse)
}

// complete is the internal implementation shared by Complete and CompleteNoThink.
// When think is non-nil, it is sent as the "think" parameter to llama-server.
// Sends a lightweight probe to verify the model is responding before the real request.
func (d *DeepThinkerClient) complete(ctx context.Context, systemPrompt, userContent string, maxTokens int, think *bool) (string, error) {
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

	payload := dtcRequest{
		Model: d.Model,
		Messages: []dtcMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature: 0.1,
		MaxTokens:   maxTokens,
		Think:       think,
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

