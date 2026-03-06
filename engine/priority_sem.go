package engine

import (
	"context"
	"sync"
)

// Priority represents the urgency of a slot acquisition request.
type Priority int

const (
	PriorityBackground Priority = iota // routines, heartbeats, scheduled tasks
	PriorityUser                       // interactive user messages
)

// PrioritySem is a counting semaphore with priority-aware wakeup and
// reservation support. When a slot is released, high-priority waiters are
// served before low-priority ones. Reservations prevent low-priority
// TryAcquireAt calls from stealing a slot that is about to be reacquired
// by a high-priority caller (the Release→Reacquire gap during tool execution).
type PrioritySem struct {
	mu       sync.Mutex
	avail    int              // unreserved available slots
	reserved int              // slots reserved for high-priority reacquire
	cap      int              // total capacity
	high     []chan struct{}   // PriorityUser waiters (FIFO)
	low      []chan struct{}   // PriorityBackground waiters (FIFO)
}

// NewPrioritySem creates a semaphore with the given number of slots.
func NewPrioritySem(slots int) *PrioritySem {
	if slots <= 0 {
		slots = 1
	}
	return &PrioritySem{avail: slots, cap: slots}
}

// TryAcquire attempts a non-blocking acquire at user priority. Returns true
// if a slot was acquired (reserved or unreserved). Used by TryAcquirePrimary
// which is always in user context.
func (s *PrioritySem) TryAcquire() bool {
	return s.TryAcquireAt(PriorityUser)
}

// TryAcquireAt attempts a non-blocking acquire at the given priority.
// Background priority cannot take reserved slots and fails if high-priority
// waiters are queued (prevents slot stealing).
func (s *PrioritySem) TryAcquireAt(pri Priority) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if pri >= PriorityUser {
		// User: can take reserved slots.
		if s.reserved > 0 {
			s.reserved--
			return true
		}
		if s.avail > 0 {
			s.avail--
			return true
		}
		return false
	}

	// Background: only unreserved slots, and not if users are waiting.
	if s.avail > 0 && len(s.high) == 0 {
		s.avail--
		return true
	}
	return false
}

// Acquire blocks until a slot is available at the given priority or ctx is
// cancelled. High-priority acquires can consume reserved slots and are woken
// before low-priority waiters.
func (s *PrioritySem) Acquire(ctx context.Context, pri Priority) error {
	s.mu.Lock()

	// Fast path.
	if pri >= PriorityUser && s.reserved > 0 {
		s.reserved--
		s.mu.Unlock()
		return nil
	}
	if s.avail > 0 {
		// Background callers also check: no high-pri waiters to cut in front of.
		if pri >= PriorityUser || len(s.high) == 0 {
			s.avail--
			s.mu.Unlock()
			return nil
		}
	}

	// Slow path: enqueue a waiter channel.
	ch := make(chan struct{})
	if pri >= PriorityUser {
		s.high = append(s.high, ch)
	} else {
		s.low = append(s.low, ch)
	}
	s.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		var removed bool
		if pri >= PriorityUser {
			removed = removeWaiterCh(&s.high, ch)
		} else {
			removed = removeWaiterCh(&s.low, ch)
		}
		s.mu.Unlock()

		if !removed {
			// Release already closed ch (we have the slot). Return it since
			// we're cancelled.
			<-ch // drain (instant — already closed)
			s.Release()
		}
		return ctx.Err()
	}
}

// Release returns a slot to the semaphore. High-priority waiters are served
// first, then low-priority, then the slot becomes available.
func (s *PrioritySem) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatch()
}

// ReleaseReserved returns a slot but reserves it for a high-priority
// reacquire. If a high-priority waiter is already queued, it is served
// immediately. Otherwise the slot is parked as reserved — invisible to
// background TryAcquireAt and Acquire calls.
func (s *PrioritySem) ReleaseReserved() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.high) > 0 {
		ch := s.high[0]
		s.high = s.high[1:]
		close(ch)
		return
	}
	s.reserved++
}

// CancelReservation cancels an outstanding reservation (e.g. when a
// high-priority Reacquire fails due to context cancellation). The freed
// slot is dispatched normally.
func (s *PrioritySem) CancelReservation() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.reserved <= 0 {
		return
	}
	s.reserved--
	// The freed reservation becomes a normal slot — dispatch it.
	s.avail++
	s.dispatch()
}

// dispatch wakes the highest-priority waiter, or increments avail.
// Must be called with mu held.
func (s *PrioritySem) dispatch() {
	if len(s.high) > 0 {
		ch := s.high[0]
		s.high = s.high[1:]
		close(ch)
		return
	}
	if len(s.low) > 0 {
		ch := s.low[0]
		s.low = s.low[1:]
		close(ch)
		return
	}
	s.avail++
}

// SlotsInUse returns the number of held slots and total capacity.
func (s *PrioritySem) SlotsInUse() (used, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cap - s.avail - s.reserved, s.cap
}

// removeWaiterCh removes a specific channel from a waiter slice.
// Returns true if found and removed, false if not present (already dispatched).
func removeWaiterCh(slice *[]chan struct{}, ch chan struct{}) bool {
	for i, w := range *slice {
		if w == ch {
			*slice = append((*slice)[:i], (*slice)[i+1:]...)
			return true
		}
	}
	return false
}
