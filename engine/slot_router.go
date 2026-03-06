package engine

import (
	"context"
	"time"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/metrics"
)

// OrchestratorChoice holds the resolved client/model for an orchestrator call.
type OrchestratorChoice struct {
	Client          *llm.Client
	Model           string
	Release         func()                          // MUST be called when the orchestrator call completes
	ReleaseReserved func()                          // release with reservation for high-priority reacquire
	Reacquire       func(ctx context.Context) error // re-acquire the same slot type after tool execution
}

// SlotRouter decides which LLM backend handles an orchestrator call.
type SlotRouter interface {
	// AcquireOrFallback resolves the orchestrator. When preferBrain is true,
	// Brain is tried first (interactive messages need accuracy); when false,
	// the primary 9B is tried first (routines, heartbeats, scheduler).
	// Priority controls waiter ordering and reservation visibility.
	AcquireOrFallback(ctx context.Context, preferBrain bool, pri Priority) OrchestratorChoice

	// TryAcquirePrimary attempts to acquire the primary (9B supervisor) slot
	// without blocking. Returns the choice and true if acquired, or a zero
	// value and false if the slot is busy. Used by the message loop to skip
	// triage when the supervisor is immediately available.
	TryAcquirePrimary() (OrchestratorChoice, bool)
}

// SlotChecker abstracts priority-aware semaphore access for orchestrator
// slot routing. Implemented by SubagentClient and DeepThinkerClient.
type SlotChecker interface {
	TryAcquire() bool
	TryAcquireAt(Priority) bool
	Acquire(ctx context.Context, pri Priority) error
	Release()
	ReleaseReserved()
	CancelReservation()
}

// slotRouter routes orchestrator calls based on priority. In two-model mode:
// primary = 9B (fast orchestrator, 3 slots), fallback = 122B Brain (deep thinker, 1 slot).
// When preferBrain=true (interactive), Brain is tried first for accuracy.
// When preferBrain=false (routines/heartbeats), 9B is tried first for throughput.
type slotRouter struct {
	primary       *llm.Client
	primaryModel  string
	fallback      *llm.Client
	fallbackModel string
	primarySlots  SlotChecker // primary server semaphore
	fallbackSlots SlotChecker // fallback server semaphore
	metrics       *metrics.Collector
}

// NewSlotRouter creates a router that routes orchestrator calls based on
// priority and slot availability.
func NewSlotRouter(primary *llm.Client, primaryModel string,
	fallback *llm.Client, fallbackModel string,
	primarySlots, fallbackSlots SlotChecker,
	m *metrics.Collector) SlotRouter {
	return &slotRouter{
		primary: primary, primaryModel: primaryModel,
		fallback: fallback, fallbackModel: fallbackModel,
		primarySlots: primarySlots, fallbackSlots: fallbackSlots,
		metrics: m,
	}
}

// brainChoice constructs an OrchestratorChoice for the Brain (fallback) slot.
// Priority determines the Reacquire urgency after tool-execution gaps.
func (r *slotRouter) brainChoice(pri Priority) OrchestratorChoice {
	return OrchestratorChoice{
		Client:          r.fallback,
		Model:           r.fallbackModel,
		Release:         r.fallbackSlots.Release,
		ReleaseReserved: r.fallbackSlots.ReleaseReserved,
		Reacquire: func(ctx context.Context) error {
			err := r.fallbackSlots.Acquire(ctx, pri)
			if err != nil {
				r.fallbackSlots.CancelReservation()
			}
			return err
		},
	}
}

// primaryChoice constructs an OrchestratorChoice for the primary (9B) slot.
// Priority determines the Reacquire urgency after tool-execution gaps.
func (r *slotRouter) primaryChoice(pri Priority) OrchestratorChoice {
	return OrchestratorChoice{
		Client:          r.primary,
		Model:           r.primaryModel,
		Release:         r.primarySlots.Release,
		ReleaseReserved: r.primarySlots.ReleaseReserved,
		Reacquire: func(ctx context.Context) error {
			err := r.primarySlots.Acquire(ctx, pri)
			if err != nil {
				r.primarySlots.CancelReservation()
			}
			return err
		},
	}
}

func (r *slotRouter) TryAcquirePrimary() (OrchestratorChoice, bool) {
	start := time.Now()
	if r.primarySlots.TryAcquire() {
		logger.Log.Debug("[router] acquired orchestrator slot (non-blocking)")
		r.metrics.Since("slot.acquire", start, map[string]string{
			"backend": "9b", "strategy": "try_primary", "result": "ok",
		})
		return r.primaryChoice(PriorityUser), true
	}
	r.metrics.Since("slot.acquire", start, map[string]string{
		"backend": "9b", "strategy": "try_primary", "result": "busy",
	})
	return OrchestratorChoice{}, false
}

func (r *slotRouter) AcquireOrFallback(ctx context.Context, preferBrain bool, pri Priority) OrchestratorChoice {
	if preferBrain {
		return r.acquirePreferBrain(ctx, pri)
	}
	return r.acquirePreferPrimary(ctx, pri)
}

// acquirePreferBrain implements the interactive strategy:
// 1. TryAcquire Brain (non-blocking) → use Brain
// 2. Brain busy → TryAcquire 9B (non-blocking) → use 9B
// 3. Both busy → block on Brain
func (r *slotRouter) acquirePreferBrain(ctx context.Context, pri Priority) OrchestratorChoice {
	start := time.Now()

	// Try Brain first (non-blocking).
	if r.fallbackSlots.TryAcquireAt(pri) {
		logger.Log.Debug("[router] acquired Brain slot (preferred)")
		r.metrics.Since("slot.acquire", start, map[string]string{
			"backend": "brain", "strategy": "prefer_brain", "result": "ok",
		})
		return r.brainChoice(pri)
	}

	// Brain busy — try primary (non-blocking).
	if r.primarySlots.TryAcquireAt(pri) {
		logger.Log.Debug("[router] Brain busy, acquired orchestrator slot")
		r.metrics.Since("slot.acquire", start, map[string]string{
			"backend": "9b", "strategy": "prefer_brain", "result": "fallback",
		})
		return r.primaryChoice(pri)
	}

	// Both busy — block on Brain at caller's priority.
	if err := r.fallbackSlots.Acquire(ctx, pri); err != nil {
		logger.Log.Warnf("[router] Brain acquire cancelled: %v, sending uncoordinated", err)
		r.metrics.Since("slot.acquire", start, map[string]string{
			"backend": "brain", "strategy": "prefer_brain", "result": "cancelled",
		})
		return r.uncoordinatedChoice()
	}

	logger.Log.Debug("[router] both busy, waited for Brain slot")
	r.metrics.Since("slot.acquire", start, map[string]string{
		"backend": "brain", "strategy": "prefer_brain", "result": "ok",
	})
	return r.brainChoice(pri)
}

// acquirePreferPrimary implements the routine/heartbeat strategy:
// 1. TryAcquireAt 9B (non-blocking, priority-aware) → use 9B
// 2. 9B busy/yielding → block on Brain at caller's priority
func (r *slotRouter) acquirePreferPrimary(ctx context.Context, pri Priority) OrchestratorChoice {
	start := time.Now()

	// Try primary (non-blocking). At background priority, this fails if a
	// user reservation exists or a user waiter is queued — preventing
	// slot stealing during the Release→Reacquire gap.
	if r.primarySlots.TryAcquireAt(pri) {
		logger.Log.Debug("[router] acquired orchestrator slot")
		r.metrics.Since("slot.acquire", start, map[string]string{
			"backend": "9b", "strategy": "prefer_primary", "result": "ok",
		})
		return r.primaryChoice(pri)
	}

	// Primary busy or yielding — wait for Brain at caller's priority.
	if err := r.fallbackSlots.Acquire(ctx, pri); err != nil {
		logger.Log.Warnf("[router] fallback acquire cancelled: %v, sending uncoordinated", err)
		r.metrics.Since("slot.acquire", start, map[string]string{
			"backend": "brain", "strategy": "prefer_primary", "result": "cancelled",
		})
		return r.uncoordinatedChoice()
	}

	logger.Log.Debug("[router] orchestrator busy, falling back to Brain")
	r.metrics.Since("slot.acquire", start, map[string]string{
		"backend": "brain", "strategy": "prefer_primary", "result": "fallback",
	})
	return r.brainChoice(pri)
}

// uncoordinatedChoice returns a fallback choice with no-op release/reacquire
// for use when context cancellation prevents proper slot acquisition.
func (r *slotRouter) uncoordinatedChoice() OrchestratorChoice {
	return OrchestratorChoice{
		Client:          r.fallback,
		Model:           r.fallbackModel,
		Release:         func() {},
		ReleaseReserved: func() {},
		Reacquire:       func(context.Context) error { return nil },
	}
}

// passthroughRouter always returns the same client/model with a no-op release.
// Used when two-model mode is not configured.
type passthroughRouter struct {
	client *llm.Client
	model  string
}

// NewPassthroughRouter creates a router that always returns the given client/model.
func NewPassthroughRouter(client *llm.Client, model string) SlotRouter {
	return &passthroughRouter{client: client, model: model}
}

func (r *passthroughRouter) TryAcquirePrimary() (OrchestratorChoice, bool) {
	return r.noopChoice(), true
}

func (r *passthroughRouter) AcquireOrFallback(_ context.Context, _ bool, _ Priority) OrchestratorChoice {
	return r.noopChoice()
}

func (r *passthroughRouter) noopChoice() OrchestratorChoice {
	return OrchestratorChoice{
		Client:          r.client,
		Model:           r.model,
		Release:         func() {},
		ReleaseReserved: func() {},
		Reacquire:       func(context.Context) error { return nil },
	}
}
