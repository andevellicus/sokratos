package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"sokratos/logger"
	"sokratos/memory"
)

// SubagentClient manages a lightweight subagent (e.g. GLM-4.7-Flash) running on
// a dedicated server. It owns a concurrency semaphore matching the server's
// --parallel setting so we never dispatch more concurrent requests than the
// server can handle.
type SubagentClient struct {
	baseClient
	sem chan struct{}
}

// NewSubagentClient returns a ready-to-use client. slots controls the max
// concurrent requests (should match the router's --n-parallel).
func NewSubagentClient(url, model string, slots int) *SubagentClient {
	return NewSubagentClientNamed("subagent", url, model, slots)
}

// NewSubagentClientNamed is like NewSubagentClient but uses a custom name for
// the circuit breaker and log tag, allowing multiple backends to be
// distinguished (e.g. "subagent-flash" vs "subagent-z1").
func NewSubagentClientNamed(name, url, model string, slots int) *SubagentClient {
	if slots <= 0 {
		slots = 2
	}
	return &SubagentClient{
		baseClient: baseClient{
			URL:    url,
			Model:  model,
			client: &http.Client{Timeout: TimeoutSubagent + 10*time.Second},
			cb:     newCircuitBreaker(name),
			logTag: "[" + name + "]",
		},
		sem: make(chan struct{}, slots),
	}
}

// acquire blocks until a semaphore slot is available or ctx is cancelled.
func (sc *SubagentClient) acquire(ctx context.Context) error {
	select {
	case sc.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release frees a semaphore slot.
func (sc *SubagentClient) release() {
	<-sc.sem
}

// Complete sends a system+user message pair and returns the raw content string.
// Thinking is disabled (structured output tasks).
func (sc *SubagentClient) Complete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return sc.complete(ctx, systemPrompt, userContent, "", maxTokens)
}

// CompleteWithGrammar is like Complete but applies a GBNF grammar constraint.
func (sc *SubagentClient) CompleteWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error) {
	return sc.complete(ctx, systemPrompt, userContent, grammar, maxTokens)
}

func (sc *SubagentClient) complete(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error) {
	if err := sc.cb.check(); err != nil {
		return "", err
	}

	if err := sc.acquire(ctx); err != nil {
		return "", fmt.Errorf("subagent semaphore: %w", err)
	}
	defer sc.release()

	if err := sc.ensureLoaded(ctx); err != nil {
		sc.cb.recordFailure()
		return "", fmt.Errorf("subagent model not available: %w", err)
	}

	payload := dtcRequest{
		Model: sc.Model,
		Messages: []dtcMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature: 0.1,
		MaxTokens:   maxTokens,
		Think:       thinkFalse,
		Grammar:     grammar,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	result, err := sc.doRequest(ctx, body)
	if err != nil {
		sc.cb.recordFailure()
		return "", err
	}

	// Treat empty responses as failures — the server is likely overloaded and
	// returned a truncated/empty generation. Recording the failure helps the
	// circuit breaker open before callers accumulate dead requests.
	if result == "" {
		sc.cb.recordFailure()
		return "", fmt.Errorf("subagent returned empty response (server may be overloaded)")
	}

	sc.cb.recordSuccess()
	return result, nil
}

// SubagentPool distributes subagent work across multiple backends (e.g. Flash
// + Z1) using round-robin with automatic fallback. Each backend has its own
// semaphore and circuit breaker, so a saturated or failing backend is skipped
// transparently.
type SubagentPool struct {
	clients []*SubagentClient
	idx     atomic.Uint64
}

// NewSubagentPool creates a pool from one or more SubagentClient backends.
func NewSubagentPool(clients ...*SubagentClient) *SubagentPool {
	return &SubagentPool{clients: clients}
}

// Complete sends a request to the next available backend. If the primary
// choice fails (circuit breaker open, semaphore full, error), it tries the
// remaining backends in round-robin order.
func (sp *SubagentPool) Complete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	n := uint64(len(sp.clients))
	start := sp.idx.Add(1) - 1
	var lastErr error
	for i := uint64(0); i < n; i++ {
		client := sp.clients[(start+i)%n]
		result, err := client.Complete(ctx, systemPrompt, userContent, maxTokens)
		if err == nil {
			return result, nil
		}
		lastErr = err
		logger.Log.Debugf("[subagent-pool] backend %s failed: %v, trying next", client.logTag, err)
		if ctx.Err() != nil {
			return "", lastErr
		}
	}
	return "", fmt.Errorf("all subagent backends failed: %w", lastErr)
}

// Func returns a memory.SubagentFunc that routes through this pool.
func (sp *SubagentPool) Func() memory.SubagentFunc {
	return func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
		return sp.Complete(ctx, systemPrompt, userPrompt, 1024)
	}
}
