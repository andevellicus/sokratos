package engine

import (
	"context"

	"sokratos/llm"
	"sokratos/logger"
)

// OrchestratorChoice holds the resolved client/model for an orchestrator call.
type OrchestratorChoice struct {
	Client    *llm.Client
	Model     string
	Release   func()                          // MUST be called when the orchestrator call completes
	Reacquire func(ctx context.Context) error // re-acquire the same slot type after tool execution
}

// SlotRouter decides which LLM backend handles an orchestrator call.
type SlotRouter interface {
	// AcquireOrFallback resolves the orchestrator. When preferBrain is true,
	// Brain is tried first (interactive messages need accuracy); when false,
	// the primary 9B is tried first (routines, heartbeats, scheduler).
	AcquireOrFallback(ctx context.Context, preferBrain bool) OrchestratorChoice

	// TryAcquirePrimary attempts to acquire the primary (9B supervisor) slot
	// without blocking. Returns the choice and true if acquired, or a zero
	// value and false if the slot is busy. Used by the message loop to skip
	// triage when the supervisor is immediately available.
	TryAcquirePrimary() (OrchestratorChoice, bool)
}

// SlotChecker abstracts semaphore access for both DTC and SubagentClient.
type SlotChecker interface {
	TryAcquire() bool
	Acquire(ctx context.Context) error
	Release()
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
}

// NewSlotRouter creates a router that routes orchestrator calls based on
// priority and slot availability.
func NewSlotRouter(primary *llm.Client, primaryModel string,
	fallback *llm.Client, fallbackModel string,
	primarySlots, fallbackSlots SlotChecker) SlotRouter {
	return &slotRouter{
		primary: primary, primaryModel: primaryModel,
		fallback: fallback, fallbackModel: fallbackModel,
		primarySlots: primarySlots, fallbackSlots: fallbackSlots,
	}
}

// brainChoice constructs an OrchestratorChoice for the Brain (fallback) slot.
func (r *slotRouter) brainChoice() OrchestratorChoice {
	return OrchestratorChoice{
		Client:  r.fallback,
		Model:   r.fallbackModel,
		Release: r.fallbackSlots.Release,
		Reacquire: func(ctx context.Context) error {
			return r.fallbackSlots.Acquire(ctx)
		},
	}
}

// primaryChoice constructs an OrchestratorChoice for the primary (9B) slot.
func (r *slotRouter) primaryChoice() OrchestratorChoice {
	return OrchestratorChoice{
		Client:  r.primary,
		Model:   r.primaryModel,
		Release: r.primarySlots.Release,
		Reacquire: func(ctx context.Context) error {
			return r.primarySlots.Acquire(ctx)
		},
	}
}

func (r *slotRouter) TryAcquirePrimary() (OrchestratorChoice, bool) {
	if r.primarySlots.TryAcquire() {
		logger.Log.Debug("[router] acquired orchestrator slot (non-blocking)")
		return r.primaryChoice(), true
	}
	return OrchestratorChoice{}, false
}

func (r *slotRouter) AcquireOrFallback(ctx context.Context, preferBrain bool) OrchestratorChoice {
	if preferBrain {
		return r.acquirePreferBrain(ctx)
	}
	return r.acquirePreferPrimary(ctx)
}

// acquirePreferBrain implements the interactive strategy:
// 1. TryAcquire Brain (non-blocking) → use Brain
// 2. Brain busy → TryAcquire 9B (non-blocking) → use 9B
// 3. Both busy → block on Brain
func (r *slotRouter) acquirePreferBrain(ctx context.Context) OrchestratorChoice {
	// Try Brain first (non-blocking).
	if r.fallbackSlots.TryAcquire() {
		logger.Log.Debug("[router] acquired Brain slot (preferred)")
		return r.brainChoice()
	}

	// Brain busy — try primary (non-blocking).
	if r.primarySlots.TryAcquire() {
		logger.Log.Debug("[router] Brain busy, acquired orchestrator slot")
		return r.primaryChoice()
	}

	// Both busy — block on Brain.
	if err := r.fallbackSlots.Acquire(ctx); err != nil {
		logger.Log.Warnf("[router] Brain acquire cancelled: %v, sending uncoordinated", err)
		return OrchestratorChoice{
			Client:    r.fallback,
			Model:     r.fallbackModel,
			Release:   func() {},
			Reacquire: func(context.Context) error { return nil },
		}
	}

	logger.Log.Debug("[router] both busy, waited for Brain slot")
	return r.brainChoice()
}

// acquirePreferPrimary implements the routine/heartbeat strategy:
// 1. TryAcquire 9B (non-blocking) → use 9B
// 2. 9B busy → block on Brain
func (r *slotRouter) acquirePreferPrimary(ctx context.Context) OrchestratorChoice {
	// Try primary first (non-blocking).
	if r.primarySlots.TryAcquire() {
		logger.Log.Debug("[router] acquired orchestrator slot")
		return r.primaryChoice()
	}

	// Primary busy — wait for Brain.
	if err := r.fallbackSlots.Acquire(ctx); err != nil {
		logger.Log.Warnf("[router] fallback acquire cancelled: %v, sending uncoordinated", err)
		return OrchestratorChoice{
			Client:    r.fallback,
			Model:     r.fallbackModel,
			Release:   func() {},
			Reacquire: func(context.Context) error { return nil },
		}
	}

	logger.Log.Debug("[router] orchestrator busy, falling back to Brain")
	return r.brainChoice()
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
	return OrchestratorChoice{
		Client:    r.client,
		Model:     r.model,
		Release:   func() {},
		Reacquire: func(context.Context) error { return nil },
	}, true
}

func (r *passthroughRouter) AcquireOrFallback(_ context.Context, _ bool) OrchestratorChoice {
	return OrchestratorChoice{
		Client:    r.client,
		Model:     r.model,
		Release:   func() {},
		Reacquire: func(context.Context) error { return nil },
	}
}
