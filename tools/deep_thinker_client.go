package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sokratos/logger"
	"sokratos/textutil"
)

// DeepThinkerClient owns the shared HTTP client, URL, and model for all
// deep-thinker interactions (triage, consolidation, consult).
type DeepThinkerClient struct {
	URL      string
	Model    string
	client   *http.Client
	OnAccess func() // called on every successful request to keep VRAM auditor informed
}

// NewDeepThinkerClient returns a ready-to-use client with a shared 120s HTTP
// transport.
func NewDeepThinkerClient(url, model string) *DeepThinkerClient {
	return &DeepThinkerClient{
		URL:    url,
		Model:  model,
		client: &http.Client{Timeout: TimeoutDeepThinker},
	}
}

// dtcRequest is the payload sent to /v1/chat/completions.
type dtcRequest struct {
	Model       string       `json:"model,omitempty"`
	Messages    []dtcMessage `json:"messages"`
	Temperature float64      `json:"temperature"`
	MaxTokens   int          `json:"max_tokens"`
	Think       *bool        `json:"think,omitempty"` // when false, disables chain-of-thought reasoning (llama-server)
}

type dtcMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type dtcResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// triageResult is the structured output from a triage call.
type triageResult struct {
	SalienceScore float64  `json:"salience_score"`
	Summary       string   `json:"summary"`
	Category      string   `json:"category"`
	Tags          []string `json:"tags"`
	Save          *bool    `json:"save,omitempty"`
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
func (d *DeepThinkerClient) complete(ctx context.Context, systemPrompt, userContent string, maxTokens int, think *bool) (string, error) {
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var raw dtcResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(raw.Choices) == 0 {
		return "", fmt.Errorf("server returned empty choices array")
	}

	// Notify the VRAM auditor that the model was just used, preventing
	// idle eviction between frequent triage/consolidation calls.
	if d.OnAccess != nil {
		d.OnAccess()
	}

	return strings.TrimSpace(raw.Choices[0].Message.Content), nil
}

// thinkFalse is a reusable pointer to false for triage requests.
var thinkFalse = func() *bool { b := false; return &b }()

// Triage calls the deep thinker with thinking disabled (structured classification
// doesn't benefit from chain-of-thought, and it cuts latency significantly).
// Strips any residual think tags and code fences, then unmarshals the JSON result.
func (d *DeepThinkerClient) Triage(ctx context.Context, systemPrompt, userContent string) (*triageResult, error) {
	content, err := d.complete(ctx, systemPrompt, userContent, 2048, thinkFalse)
	if err != nil {
		return nil, err
	}

	content = textutil.StripThinkTags(content)
	content = textutil.StripCodeFences(content)
	content = textutil.ExtractJSON(content)

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

