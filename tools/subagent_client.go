package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"sokratos/httputil"
	"sokratos/logger"
	"sokratos/memory"
)

// SubagentClient manages a lightweight subagent (e.g. GLM-4.7-Flash) running on
// a dedicated server. It owns a concurrency semaphore matching the server's
// --parallel setting so we never dispatch more concurrent requests than the
// server can handle. Background work is submitted via QueueWork and processed
// sequentially — each item gets a fresh context so queue wait time doesn't eat
// into inference time.
type SubagentClient struct {
	baseClient
	sem    chan struct{}
	workCh chan memory.WorkRequest
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
	sc := &SubagentClient{
		baseClient: baseClient{
			URL:   url,
			Model: model,
			client: httputil.NewClient(TimeoutHTTPSafetyNet),
			cb:     newCircuitBreaker(name),
			logTag: "[" + name + "]",
		},
		sem:    make(chan struct{}, slots),
		workCh: make(chan memory.WorkRequest, 64),
	}
	// Start one worker per slot so idle capacity gets used for background work.
	for i := 0; i < slots; i++ {
		go sc.processWorkQueue()
	}
	return sc
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

// tryAcquire attempts to acquire a semaphore slot without blocking. Returns
// false immediately if all slots are occupied.
func (sc *SubagentClient) tryAcquire() bool {
	select {
	case sc.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Complete sends a system+user message pair and returns the raw content string.
// Thinking is disabled (structured output tasks).
func (sc *SubagentClient) Complete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return sc.complete(ctx, systemPrompt, userContent, "", maxTokens)
}

// TryComplete is like Complete but returns immediately with an error if the
// semaphore is full (all slots occupied). Use for optional background work
// that should not queue behind critical requests.
func (sc *SubagentClient) TryComplete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	if err := sc.cb.check(); err != nil {
		return "", err
	}
	if !sc.tryAcquire() {
		return "", fmt.Errorf("subagent %s busy (all slots occupied)", sc.logTag)
	}
	defer sc.release()

	if err := sc.ensureLoaded(ctx); err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", fmt.Errorf("subagent model not available: %w", err)
	}

	payload := dtcRequest{
		Model: sc.Model,
		Messages: []dtcMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature:     0.1,
		MaxTokens:       maxTokens,
		Think:           thinkFalse,
		ReasoningFormat: "deepseek",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	result, err := sc.doRequest(ctx, body)
	if err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", err
	}

	if result == "" {
		sc.cb.recordFailure()
		return "", fmt.Errorf("subagent returned empty response (server may be overloaded)")
	}

	sc.cb.recordSuccess()
	return result, nil
}

// CompleteWithGrammar is like Complete but applies a GBNF grammar constraint.
func (sc *SubagentClient) CompleteWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error) {
	return sc.complete(ctx, systemPrompt, userContent, grammar, maxTokens)
}

// TryCompleteWithGrammar is like TryComplete but applies a GBNF grammar
// constraint. Returns immediately with an error if all slots are occupied.
func (sc *SubagentClient) TryCompleteWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error) {
	if err := sc.cb.check(); err != nil {
		return "", err
	}
	if !sc.tryAcquire() {
		return "", fmt.Errorf("subagent %s busy (all slots occupied)", sc.logTag)
	}
	defer sc.release()

	if err := sc.ensureLoaded(ctx); err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", fmt.Errorf("subagent model not available: %w", err)
	}

	payload := dtcRequest{
		Model: sc.Model,
		Messages: []dtcMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature:     0.1,
		MaxTokens:       maxTokens,
		Think:           thinkFalse,
		Grammar:         grammar,
		ReasoningFormat: "deepseek",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	result, err := sc.doRequest(ctx, body)
	if err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", err
	}

	if result == "" {
		sc.cb.recordFailure()
		return "", fmt.Errorf("subagent returned empty response (server may be overloaded)")
	}

	sc.cb.recordSuccess()
	return result, nil
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
		sc.cb.recordFailureIfServer(err)
		return "", fmt.Errorf("subagent model not available: %w", err)
	}

	payload := dtcRequest{
		Model: sc.Model,
		Messages: []dtcMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature:     0.1,
		MaxTokens:       maxTokens,
		Think:           thinkFalse,
		Grammar:         grammar,
		ReasoningFormat: "deepseek",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	result, err := sc.doRequest(ctx, body)
	if err != nil {
		sc.cb.recordFailureIfServer(err)
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

// QueueWork submits a background LLM task. Items are processed sequentially
// as server slots become available. Each item gets a fresh context with
// item.Timeout, so queue wait time doesn't eat into inference time.
func (sc *SubagentClient) QueueWork(item memory.WorkRequest) {
	select {
	case sc.workCh <- item:
		logger.Log.Debugf("%s queued: %s (depth=%d/%d)", sc.logTag, item.Label, len(sc.workCh), cap(sc.workCh))
	default:
		logger.Log.Warnf("%s work queue full (cap=%d), dropping: %s", sc.logTag, cap(sc.workCh), item.Label)
		if item.OnComplete != nil {
			item.OnComplete("", fmt.Errorf("work queue full"))
		}
	}
}

// processWorkQueue drains the work channel, executing items through the normal
// semaphore-gated complete() path. Multiple goroutines run this concurrently
// (one per slot). Each item gets its own fresh context so queue wait time
// doesn't count against the inference timeout. On transient failure, items
// with Retries > 0 are requeued after a brief backoff.
func (sc *SubagentClient) processWorkQueue() {
	for item := range sc.workCh {
		ctx, cancel := context.WithTimeout(context.Background(), item.Timeout)
		result, err := sc.complete(ctx, item.SystemPrompt, item.UserPrompt, item.Grammar, item.MaxTokens)
		cancel()

		if err != nil && item.Retries > 0 {
			item.Retries--
			backoff := 2 * time.Second
			logger.Log.Warnf("%s %s failed (%v), retrying in %v (%d left)",
				sc.logTag, item.Label, err, backoff, item.Retries)
			time.Sleep(backoff)
			// Non-blocking requeue — if the channel is full, fall through
			// to OnComplete with the error rather than blocking the worker.
			select {
			case sc.workCh <- item:
				continue
			default:
				logger.Log.Warnf("%s %s retry failed: queue full, delivering error", sc.logTag, item.Label)
			}
		}

		if item.OnComplete != nil {
			item.OnComplete(result, err)
		}
	}
}

// CompleteMultiTurnNoReasoning sends a full message array with a GBNF grammar
// constraint but WITHOUT reasoning_format. This forces Flash to put all output
// into the content field where grammar enforcement is applied, avoiding the
// reasoning_content bypass that causes prose instead of structured JSON.
func (sc *SubagentClient) CompleteMultiTurnNoReasoning(ctx context.Context, messages []dtcMessage, grammar string, maxTokens int) (string, error) {
	if err := sc.cb.check(); err != nil {
		return "", err
	}

	if err := sc.acquire(ctx); err != nil {
		return "", fmt.Errorf("subagent semaphore: %w", err)
	}
	defer sc.release()

	if err := sc.ensureLoaded(ctx); err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", fmt.Errorf("subagent model not available: %w", err)
	}

	payload := dtcRequest{
		Model:       sc.Model,
		Messages:    messages,
		Temperature: 0.1,
		MaxTokens:   maxTokens,
		Think:       thinkFalse,
		Grammar:     grammar,
		// No ReasoningFormat — forces all output into content field.
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	result, err := sc.doRequest(ctx, body)
	if err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", err
	}

	if result == "" {
		sc.cb.recordFailure()
		return "", fmt.Errorf("subagent returned empty response (server may be overloaded)")
	}

	sc.cb.recordSuccess()
	return result, nil
}

// CompleteMultiTurnWithGrammar sends a full message array (for multi-turn tool
// execution) with a GBNF grammar constraint. Unlike Complete, this accepts
// arbitrary message sequences instead of just system+user.
func (sc *SubagentClient) CompleteMultiTurnWithGrammar(ctx context.Context, messages []dtcMessage, grammar string, maxTokens int) (string, error) {
	if err := sc.cb.check(); err != nil {
		return "", err
	}

	if err := sc.acquire(ctx); err != nil {
		return "", fmt.Errorf("subagent semaphore: %w", err)
	}
	defer sc.release()

	if err := sc.ensureLoaded(ctx); err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", fmt.Errorf("subagent model not available: %w", err)
	}

	payload := dtcRequest{
		Model:           sc.Model,
		Messages:        messages,
		Temperature:     0.1,
		MaxTokens:       maxTokens,
		Think:           thinkFalse,
		Grammar:         grammar,
		ReasoningFormat: "deepseek",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	result, err := sc.doRequest(ctx, body)
	if err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", err
	}

	if result == "" {
		sc.cb.recordFailure()
		return "", fmt.Errorf("subagent returned empty response (server may be overloaded)")
	}

	sc.cb.recordSuccess()
	return result, nil
}

