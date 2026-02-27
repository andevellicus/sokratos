package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sokratos/logger"
	"sokratos/textutil"
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
			client: &http.Client{Timeout: TimeoutDeepThinker},
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
// reasoning. Use for structured output tasks (JSON generation, classification)
// where thinking wastes tokens and risks leaking reasoning into the output.
func (d *DeepThinkerClient) CompleteNoThink(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return d.complete(ctx, systemPrompt, userContent, maxTokens, thinkFalse)
}

// complete is the internal implementation shared by Complete and Triage.
// When think is non-nil, it is sent as the "think" parameter to llama-server,
// allowing triage calls to disable chain-of-thought reasoning for lower latency.
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
		d.cb.recordFailure()
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
		d.cb.recordFailure()
		return "", err
	}

	d.cb.recordSuccess()
	return result, nil
}

// Triage calls the deep thinker with thinking disabled (structured classification
// doesn't benefit from chain-of-thought, and it cuts latency significantly).
// Strips any residual think tags and code fences, then unmarshals the JSON result.
func (d *DeepThinkerClient) Triage(ctx context.Context, systemPrompt, userContent string) (*triageResult, error) {
	content, err := d.complete(ctx, systemPrompt, userContent, 2048, thinkFalse)
	if err != nil {
		return nil, err
	}

	content = textutil.CleanLLMJSON(content)

	var result triageResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("parse triage JSON: %w (raw: %s)", err, content)
	}
	return &result, nil
}

// TriageItem truncates formattedText to maxLen, then calls Triage. This is the
// common entry point for email, calendar, and conversation triage.
// On parse errors (model returned non-JSON), returns fallback defaults instead
// of propagating the error. Network/HTTP errors still propagate.
func (d *DeepThinkerClient) TriageItem(ctx context.Context, systemPrompt, formattedText string, maxLen int) (*triageResult, error) {
	if len(formattedText) > maxLen {
		formattedText = formattedText[:maxLen] + "..."
	}
	result, err := d.Triage(ctx, systemPrompt, formattedText)
	if err != nil && strings.Contains(err.Error(), "parse triage JSON") {
		// Model returned non-JSON — degrade gracefully with safe defaults.
		logger.Log.Warnf("[triage] parse failure, using fallback defaults: %v", err)
		summary := formattedText
		if len(summary) > 200 {
			summary = summary[:200] + "..."
		}
		return &triageResult{
			SalienceScore: 5,
			Summary:       summary,
			Tags:          nil,
		}, nil
	}
	return result, err
}
