package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPrioritySem_TryAcquire(t *testing.T) {
	s := NewPrioritySem(2)

	if !s.TryAcquire() {
		t.Fatal("first TryAcquire should succeed")
	}
	if !s.TryAcquire() {
		t.Fatal("second TryAcquire should succeed")
	}
	if s.TryAcquire() {
		t.Fatal("third TryAcquire should fail (all slots busy)")
	}

	s.Release()
	if !s.TryAcquire() {
		t.Fatal("TryAcquire should succeed after Release")
	}
}

func TestPrioritySem_PriorityOrdering(t *testing.T) {
	s := NewPrioritySem(1)
	s.TryAcquire() // exhaust the slot

	// Queue a low-priority waiter, then a high-priority waiter.
	lowGot := make(chan struct{})
	highGot := make(chan struct{})

	go func() {
		s.Acquire(context.Background(), PriorityBackground)
		close(lowGot)
	}()
	go func() {
		s.Acquire(context.Background(), PriorityUser)
		close(highGot)
	}()

	// Let both goroutines enqueue.
	time.Sleep(20 * time.Millisecond)

	// Release one slot — high-priority should wake first.
	s.Release()
	select {
	case <-highGot:
		// expected
	case <-lowGot:
		t.Fatal("low-priority woke before high-priority")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for high-priority waiter")
	}

	// Release again — now low-priority should wake.
	s.Release()
	select {
	case <-lowGot:
		// expected
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for low-priority waiter")
	}
}

func TestPrioritySem_Reservation(t *testing.T) {
	s := NewPrioritySem(1)
	s.TryAcquire() // slot held

	// ReleaseReserved: slot returned but reserved for high-priority.
	s.ReleaseReserved()

	// Background TryAcquireAt should fail — slot is reserved.
	if s.TryAcquireAt(PriorityBackground) {
		t.Fatal("background TryAcquireAt should fail when reservation exists")
	}

	// User TryAcquireAt should succeed — consumes reservation.
	if !s.TryAcquireAt(PriorityUser) {
		t.Fatal("user TryAcquireAt should succeed against reservation")
	}

	used, total := s.SlotsInUse()
	if used != 1 || total != 1 {
		t.Fatalf("expected 1/1 slots in use, got %d/%d", used, total)
	}
}

func TestPrioritySem_ReservationWakesHighWaiter(t *testing.T) {
	s := NewPrioritySem(1)
	s.TryAcquire() // slot held

	// Queue a high-priority waiter.
	highGot := make(chan struct{})
	go func() {
		s.Acquire(context.Background(), PriorityUser)
		close(highGot)
	}()
	time.Sleep(20 * time.Millisecond)

	// ReleaseReserved should serve the high-priority waiter immediately.
	s.ReleaseReserved()

	select {
	case <-highGot:
		// expected
	case <-time.After(time.Second):
		t.Fatal("ReleaseReserved should have woken high-priority waiter")
	}
}

func TestPrioritySem_CancelReservation(t *testing.T) {
	s := NewPrioritySem(1)
	s.TryAcquire()

	s.ReleaseReserved() // creates reservation

	// Cancel the reservation.
	s.CancelReservation()

	// Now background TryAcquireAt should succeed.
	if !s.TryAcquireAt(PriorityBackground) {
		t.Fatal("background TryAcquireAt should succeed after CancelReservation")
	}
}

func TestPrioritySem_CancelReservationWakesLowWaiter(t *testing.T) {
	s := NewPrioritySem(1)
	s.TryAcquire() // slot held

	// Queue a low-priority waiter.
	lowGot := make(chan struct{})
	go func() {
		s.Acquire(context.Background(), PriorityBackground)
		close(lowGot)
	}()
	time.Sleep(20 * time.Millisecond)

	// ReleaseReserved — won't wake low-priority (it's reserved).
	s.ReleaseReserved()

	select {
	case <-lowGot:
		t.Fatal("low-priority should not wake on ReleaseReserved")
	case <-time.After(50 * time.Millisecond):
		// expected
	}

	// CancelReservation — should wake low-priority.
	s.CancelReservation()
	select {
	case <-lowGot:
		// expected
	case <-time.After(time.Second):
		t.Fatal("CancelReservation should have woken low-priority waiter")
	}
}

func TestPrioritySem_BackgroundBlockedByHighWaiters(t *testing.T) {
	s := NewPrioritySem(1)

	// Queue a high-priority waiter while slot is free — it should get the slot immediately.
	// Instead, test: hold slot, queue high-pri, release, try background.
	s.TryAcquire()

	highGot := make(chan struct{})
	go func() {
		s.Acquire(context.Background(), PriorityUser)
		close(highGot)
	}()
	time.Sleep(20 * time.Millisecond)

	s.Release()
	<-highGot // high got the slot

	// Now high holds it. Release again.
	s.Release()

	// Background TryAcquireAt should succeed (no high waiters now).
	if !s.TryAcquireAt(PriorityBackground) {
		t.Fatal("background TryAcquireAt should succeed when no high waiters")
	}
}

func TestPrioritySem_AcquireCancellation(t *testing.T) {
	s := NewPrioritySem(1)
	s.TryAcquire() // exhaust

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := s.Acquire(ctx, PriorityUser)
	if err == nil {
		t.Fatal("Acquire with cancelled context should fail")
	}

	// Slot should still be held (not leaked).
	used, _ := s.SlotsInUse()
	if used != 1 {
		t.Fatalf("expected 1 slot in use, got %d", used)
	}
}

func TestPrioritySem_AcquireCancelledWhileSignaled(t *testing.T) {
	s := NewPrioritySem(1)
	s.TryAcquire() // exhaust

	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		close(started)
		errCh <- s.Acquire(ctx, PriorityUser)
	}()
	<-started
	time.Sleep(20 * time.Millisecond)

	// Cancel and release near-simultaneously — the slot should not leak.
	cancel()
	s.Release()

	<-errCh
	// One of two outcomes: Acquire succeeded then slot was released by us,
	// or Acquire was cancelled and Release returned the slot.
	// Either way, after settling, the slot should be available.
	time.Sleep(20 * time.Millisecond)
	used, _ := s.SlotsInUse()
	if used != 0 {
		t.Fatalf("expected 0 slots in use after settle, got %d", used)
	}
}

func TestPrioritySem_BackgroundAcquireYieldsToHighWaiters(t *testing.T) {
	s := NewPrioritySem(2)

	// Exhaust both slots.
	s.TryAcquire()
	s.TryAcquire()

	// Queue a high-priority waiter and a low-priority waiter.
	highGot := make(chan struct{})
	lowGot := make(chan struct{})
	go func() {
		s.Acquire(context.Background(), PriorityUser)
		close(highGot)
	}()
	go func() {
		s.Acquire(context.Background(), PriorityBackground)
		close(lowGot)
	}()
	time.Sleep(20 * time.Millisecond)

	// Release one — high should get it.
	s.Release()
	<-highGot

	// Release another — now low gets it.
	s.Release()
	<-lowGot
}

func TestPrioritySem_ConcurrentSafety(t *testing.T) {
	s := NewPrioritySem(3)
	var wg sync.WaitGroup

	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pri := PriorityBackground
			if time.Now().UnixNano()%2 == 0 {
				pri = PriorityUser
			}
			if err := s.Acquire(context.Background(), pri); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
			s.Release()
		}()
	}
	wg.Wait()

	used, _ := s.SlotsInUse()
	if used != 0 {
		t.Fatalf("expected 0 slots in use after all done, got %d", used)
	}
}
