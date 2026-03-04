package clients

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/httputil"
	"sokratos/memory"
	"sokratos/timeouts"
)

// SubagentClient manages a lightweight subagent running on a dedicated server.
// It owns a concurrency semaphore matching the server's
// --parallel setting so we never dispatch more concurrent requests than the
// server can handle. Background work is submitted via QueueWork and processed
// sequentially — each item gets a fresh context so queue wait time doesn't eat
// into inference time.
type SubagentClient struct {
	baseClient
	sem chan struct{}
	wq  *WorkQueue
}

// NewSubagentClientNamed returns a ready-to-use client with a custom name for
// the circuit breaker and log tag, allowing multiple backends to be
// distinguished (e.g. "subagent-flash" vs "subagent-z1"). slots controls the
// max concurrent requests (should match the router's --n-parallel).
func NewSubagentClientNamed(name, url, model string, slots int) *SubagentClient {
	if slots <= 0 {
		slots = 2
	}
	sc := &SubagentClient{
		baseClient: baseClient{
			URL:   url,
			Model: model,
			client: httputil.NewClient(timeouts.HTTPSafetyNet),
			cb:     newCircuitBreaker(name),
			logTag: "[" + name + "]",
		},
		sem: make(chan struct{}, slots),
	}
	sc.wq = NewWorkQueue(64, slots, sc.logTag, func(ctx context.Context, req memory.WorkRequest) (string, error) {
		return sc.exec(ctx, requestOpts{
			system: req.SystemPrompt, user: req.UserPrompt,
			grammar: req.Grammar, maxTokens: req.MaxTokens,
		})
	})
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

// SlotsInUse returns the number of currently occupied semaphore slots and the
// total capacity. Useful for logging and diagnostics.
func (sc *SubagentClient) SlotsInUse() (used, total int) {
	return len(sc.sem), cap(sc.sem)
}

// Acquire blocks until a semaphore slot is available or ctx is cancelled.
// Exported for the slot router to reserve a slot for orchestrator fallback.
func (sc *SubagentClient) Acquire(ctx context.Context) error {
	return sc.acquire(ctx)
}

// TryAcquire attempts to acquire a semaphore slot without blocking.
// Exported for the slot router.
func (sc *SubagentClient) TryAcquire() bool {
	return sc.tryAcquire()
}

// Release frees a semaphore slot. Exported for the slot router.
func (sc *SubagentClient) Release() {
	sc.release()
}

// requestOpts captures the variation axes for exec(): blocking vs non-blocking
// acquire, pre-built messages vs system+user, and optional grammar.
type requestOpts struct {
	messages  []chatMessage // if set, use directly; else build from system+user
	system    string
	user      string
	grammar   string
	maxTokens int
	tryOnly   bool // non-blocking acquire
	thinking  bool // enable_thinking + reasoning_format="deepseek"
}

// exec is the single internal method that all public methods delegate to.
// It handles: circuit breaker → acquire → ensureLoaded → build payload →
// marshal → doRequest → empty check → CB recording.
func (sc *SubagentClient) exec(ctx context.Context, opts requestOpts) (string, error) {
	if err := sc.cb.check(); err != nil {
		return "", err
	}

	if opts.tryOnly {
		if !sc.tryAcquire() {
			return "", fmt.Errorf("subagent %s busy (all slots occupied)", sc.logTag)
		}
	} else {
		if err := sc.acquire(ctx); err != nil {
			return "", fmt.Errorf("subagent semaphore: %w", err)
		}
	}
	defer sc.release()

	if err := sc.ensureLoaded(ctx); err != nil {
		sc.cb.recordFailureIfServer(err)
		return "", fmt.Errorf("subagent model not available: %w", err)
	}

	msgs := opts.messages
	if msgs == nil {
		msgs = []chatMessage{
			{Role: "system", Content: opts.system},
			{Role: "user", Content: opts.user},
		}
	}

	req := chatRequest{
		Model:       sc.Model,
		Messages:    msgs,
		Temperature: 0.1,
		MaxTokens:   opts.maxTokens,
		Grammar:     opts.grammar,
		ChatTemplateKwargs: map[string]any{
			"enable_thinking": opts.thinking,
		},
	}
	if opts.thinking {
		req.ReasoningFormat = "deepseek"
	}
	body, err := json.Marshal(req)
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

// Complete sends a system+user message pair and returns the raw content string.
func (sc *SubagentClient) Complete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return sc.exec(ctx, requestOpts{system: systemPrompt, user: userContent, maxTokens: maxTokens})
}

// TryComplete is like Complete but returns immediately with an error if the
// semaphore is full (all slots occupied). Use for optional background work
// that should not queue behind critical requests.
func (sc *SubagentClient) TryComplete(ctx context.Context, systemPrompt, userContent string, maxTokens int) (string, error) {
	return sc.exec(ctx, requestOpts{system: systemPrompt, user: userContent, maxTokens: maxTokens, tryOnly: true})
}

// CompleteWithGrammar is like Complete but applies a GBNF grammar constraint.
func (sc *SubagentClient) CompleteWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error) {
	return sc.exec(ctx, requestOpts{system: systemPrompt, user: userContent, grammar: grammar, maxTokens: maxTokens})
}

// TryCompleteWithGrammar is like TryComplete but applies a GBNF grammar
// constraint. Returns immediately with an error if all slots are occupied.
func (sc *SubagentClient) TryCompleteWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error) {
	return sc.exec(ctx, requestOpts{system: systemPrompt, user: userContent, grammar: grammar, maxTokens: maxTokens, tryOnly: true})
}

// TryCompleteWithGrammarThinking is like TryCompleteWithGrammar but enables
// chain-of-thought reasoning (thinking tokens go into reasoning_content,
// grammar applies only to content tokens). Returns immediately if all slots
// are occupied.
func (sc *SubagentClient) TryCompleteWithGrammarThinking(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error) {
	return sc.exec(ctx, requestOpts{
		system: systemPrompt, user: userContent,
		grammar: grammar, maxTokens: maxTokens,
		tryOnly: true, thinking: true,
	})
}

// CompleteMultiTurnWithGrammar sends a full message array (for multi-turn tool
// execution) with a GBNF grammar constraint. Unlike Complete, this accepts
// arbitrary message sequences instead of just system+user.
func (sc *SubagentClient) CompleteMultiTurnWithGrammar(ctx context.Context, messages []chatMessage, grammar string, maxTokens int) (string, error) {
	return sc.exec(ctx, requestOpts{messages: messages, grammar: grammar, maxTokens: maxTokens})
}

// CaptionImage sends a multimodal (text + image) message to the subagent and
// returns a free-form text caption. Uses tryOnly (non-blocking semaphore) so
// it returns immediately if all slots are busy.
func (sc *SubagentClient) CaptionImage(ctx context.Context, systemPrompt, imageDataURI string, maxTokens int) (string, error) {
	return sc.exec(ctx, requestOpts{
		messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{
				Role: "user",
				Parts: []contentPart{
					{Type: "text", Text: "Describe this image concisely."},
					{Type: "image_url", ImageURL: &imageURL{URL: imageDataURI}},
				},
			},
		},
		maxTokens: maxTokens,
		tryOnly:   true,
	})
}

// QueueWork submits a background LLM task to the shared work queue.
func (sc *SubagentClient) QueueWork(item memory.WorkRequest) {
	sc.wq.QueueWork(item)
}
