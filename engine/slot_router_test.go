package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// newTestSem creates a PrioritySem for testing (convenience wrapper).
func newTestSem(slots int) *PrioritySem {
	return NewPrioritySem(slots)
}

// TestAcquirePreferBrain_BrainFree verifies that when preferBrain=true and
// Brain has a free slot, Brain is returned.
func TestAcquirePreferBrain_BrainFree(t *testing.T) {
	primarySlots := newTestSem(3)
	brainSlots := newTestSem(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)

	choice := router.AcquireOrFallback(context.Background(), true, PriorityUser)

	if choice.Model != "brain-model" {
		t.Fatalf("expected brain-model, got %s", choice.Model)
	}
	used, _ := brainSlots.SlotsInUse()
	if used != 1 {
		t.Fatalf("expected 1 brain slot in use, got %d", used)
	}
	pUsed, _ := primarySlots.SlotsInUse()
	if pUsed != 0 {
		t.Fatalf("expected 0 primary slots in use, got %d", pUsed)
	}

	choice.Release()
	used, _ = brainSlots.SlotsInUse()
	if used != 0 {
		t.Fatalf("expected 0 brain slots in use after release, got %d", used)
	}
}

// TestAcquirePreferBrain_BrainBusy9BFree verifies that when preferBrain=true
// and Brain is busy but 9B has a slot, 9B is returned as fallback.
func TestAcquirePreferBrain_BrainBusy9BFree(t *testing.T) {
	primarySlots := newTestSem(3)
	brainSlots := newTestSem(1)

	brainSlots.TryAcquire() // exhaust Brain

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)
	choice := router.AcquireOrFallback(context.Background(), true, PriorityUser)

	if choice.Model != "primary-model" {
		t.Fatalf("expected primary-model fallback, got %s", choice.Model)
	}
	pUsed, _ := primarySlots.SlotsInUse()
	if pUsed != 1 {
		t.Fatalf("expected 1 primary slot in use, got %d", pUsed)
	}

	choice.Release()
	pUsed, _ = primarySlots.SlotsInUse()
	if pUsed != 0 {
		t.Fatalf("expected 0 primary slots in use after release, got %d", pUsed)
	}
}

// TestAcquirePreferBrain_BothBusy verifies that when preferBrain=true and
// both are busy, it blocks on Brain until a slot is freed.
func TestAcquirePreferBrain_BothBusy(t *testing.T) {
	primarySlots := newTestSem(1)
	brainSlots := newTestSem(1)

	primarySlots.TryAcquire()
	brainSlots.TryAcquire()

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)

	done := make(chan OrchestratorChoice, 1)
	go func() {
		choice := router.AcquireOrFallback(context.Background(), true, PriorityUser)
		done <- choice
	}()

	select {
	case <-done:
		t.Fatal("should not have returned yet")
	case <-time.After(50 * time.Millisecond):
	}

	brainSlots.Release()

	select {
	case choice := <-done:
		if choice.Model != "brain-model" {
			t.Fatalf("expected brain-model after unblock, got %s", choice.Model)
		}
		choice.Release()
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for acquisition")
	}
}

// TestAcquirePrefer9B verifies that preferBrain=false tries 9B first.
func TestAcquirePrefer9B(t *testing.T) {
	primarySlots := newTestSem(3)
	brainSlots := newTestSem(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)
	choice := router.AcquireOrFallback(context.Background(), false, PriorityBackground)

	if choice.Model != "primary-model" {
		t.Fatalf("expected primary-model, got %s", choice.Model)
	}

	choice.Release()
}

// TestAcquirePrefer9B_PrimaryBusy verifies that preferBrain=false falls back
// to Brain when primary is busy.
func TestAcquirePrefer9B_PrimaryBusy(t *testing.T) {
	primarySlots := newTestSem(1)
	brainSlots := newTestSem(1)

	primarySlots.TryAcquire() // exhaust

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)
	choice := router.AcquireOrFallback(context.Background(), false, PriorityBackground)

	if choice.Model != "brain-model" {
		t.Fatalf("expected brain-model fallback, got %s", choice.Model)
	}

	choice.Release()
}

// TestReacquire_SameSlotType verifies that Reacquire gets the same slot type
// that was originally acquired.
func TestReacquire_SameSlotType(t *testing.T) {
	primarySlots := newTestSem(3)
	brainSlots := newTestSem(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)

	t.Run("brain_reacquire", func(t *testing.T) {
		choice := router.AcquireOrFallback(context.Background(), true, PriorityUser)
		if choice.Model != "brain-model" {
			t.Fatalf("expected brain-model, got %s", choice.Model)
		}

		choice.Release()

		if err := choice.Reacquire(context.Background()); err != nil {
			t.Fatalf("reacquire failed: %v", err)
		}

		choice.Release()
	})

	t.Run("primary_reacquire", func(t *testing.T) {
		choice := router.AcquireOrFallback(context.Background(), false, PriorityUser)
		if choice.Model != "primary-model" {
			t.Fatalf("expected primary-model, got %s", choice.Model)
		}

		choice.Release()

		if err := choice.Reacquire(context.Background()); err != nil {
			t.Fatalf("reacquire failed: %v", err)
		}

		choice.Release()
	})
}

// TestPassthrough_IgnoresPreferBrain verifies that the passthrough router
// always returns the same client/model regardless of preferBrain.
func TestPassthrough_IgnoresPreferBrain(t *testing.T) {
	router := NewPassthroughRouter(nil, "passthrough-model")

	for _, preferBrain := range []bool{true, false} {
		choice := router.AcquireOrFallback(context.Background(), preferBrain, PriorityUser)
		if choice.Model != "passthrough-model" {
			t.Fatalf("preferBrain=%v: expected passthrough-model, got %s", preferBrain, choice.Model)
		}
		choice.Release()
		if err := choice.Reacquire(context.Background()); err != nil {
			t.Fatalf("preferBrain=%v: reacquire failed: %v", preferBrain, err)
		}
	}
}

// TestReacquire_ContextCancelled verifies that Reacquire respects context
// cancellation and cleans up any reservation.
func TestReacquire_ContextCancelled(t *testing.T) {
	primarySlots := newTestSem(1)
	brainSlots := newTestSem(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)

	choice := router.AcquireOrFallback(context.Background(), false, PriorityUser)
	if choice.Model != "primary-model" {
		t.Fatalf("expected primary-model, got %s", choice.Model)
	}

	// Simulate tool execution: ReleaseReserved + Reacquire with cancelled ctx.
	choice.ReleaseReserved()
	primarySlots.TryAcquire() // exhaust (simulate something else taking the slot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := choice.Reacquire(ctx)
	if err == nil {
		t.Fatal("expected reacquire to fail with cancelled context")
	}

	// Reservation should be cleaned up — background can now acquire.
	primarySlots.Release() // release what we took
	if !primarySlots.TryAcquireAt(PriorityBackground) {
		t.Fatal("background should be able to acquire after reservation cancelled")
	}
	primarySlots.Release()
}

// TestPriorityYield_BackgroundSkipsReserved verifies that background callers
// cannot steal a slot that has a reservation (the Release→Reacquire gap).
func TestPriorityYield_BackgroundSkipsReserved(t *testing.T) {
	primarySlots := newTestSem(1)
	brainSlots := newTestSem(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)

	// User acquires primary.
	choice := router.AcquireOrFallback(context.Background(), false, PriorityUser)
	if choice.Model != "primary-model" {
		t.Fatalf("expected primary-model, got %s", choice.Model)
	}

	// Simulate OnToolStart: release with reservation.
	choice.ReleaseReserved()

	// Background routine tries to acquire primary — should fail (reserved).
	bgChoice := router.AcquireOrFallback(context.Background(), false, PriorityBackground)
	if bgChoice.Model != "brain-model" {
		t.Fatalf("expected brain-model (background should yield reserved primary), got %s", bgChoice.Model)
	}
	bgChoice.Release()

	// User reacquires — should succeed (consumes reservation).
	if err := choice.Reacquire(context.Background()); err != nil {
		t.Fatalf("user reacquire failed: %v", err)
	}
	choice.Release()
}

// TestPriorityYield_BackgroundSkipsWhenUserWaiting verifies that background
// TryAcquireAt fails when a high-priority waiter is queued.
func TestPriorityYield_BackgroundSkipsWhenUserWaiting(t *testing.T) {
	primarySlots := newTestSem(1)
	brainSlots := newTestSem(1)

	// Exhaust primary (held by something).
	primarySlots.TryAcquire()

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)

	// Queue a user acquire on primary (blocking).
	userGot := make(chan struct{})
	go func() {
		// This blocks because primary is exhausted.
		primarySlots.Acquire(context.Background(), PriorityUser)
		close(userGot)
	}()
	time.Sleep(20 * time.Millisecond)

	// Background tries — should skip primary (user is waiting) and go to Brain.
	bgChoice := router.AcquireOrFallback(context.Background(), false, PriorityBackground)
	if bgChoice.Model != "brain-model" {
		t.Fatalf("expected brain-model (user waiting on primary), got %s", bgChoice.Model)
	}
	bgChoice.Release()

	// Free primary — user should get it.
	primarySlots.Release()
	select {
	case <-userGot:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for user to get primary")
	}
	primarySlots.Release()
}

// TestConcurrentAcquireRelease verifies no races under concurrent access.
func TestConcurrentAcquireRelease(t *testing.T) {
	primarySlots := newTestSem(3)
	brainSlots := newTestSem(1)
	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots, nil)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			choice := router.AcquireOrFallback(context.Background(), true, PriorityUser)
			time.Sleep(time.Millisecond)
			choice.Release()
		}()
	}
	wg.Wait()

	pUsed, _ := primarySlots.SlotsInUse()
	bUsed, _ := brainSlots.SlotsInUse()
	if pUsed != 0 {
		t.Fatalf("expected 0 primary slots in use, got %d", pUsed)
	}
	if bUsed != 0 {
		t.Fatalf("expected 0 brain slots in use, got %d", bUsed)
	}
}
