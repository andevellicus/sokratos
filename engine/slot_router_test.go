package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// mockSlotChecker is a test double for SlotChecker that uses a buffered
// channel as a counting semaphore, matching the real semaphore behavior.
type mockSlotChecker struct {
	sem chan struct{}
}

func newMockSlotChecker(slots int) *mockSlotChecker {
	ch := make(chan struct{}, slots)
	for range slots {
		ch <- struct{}{}
	}
	return &mockSlotChecker{sem: ch}
}

func (m *mockSlotChecker) TryAcquire() bool {
	select {
	case <-m.sem:
		return true
	default:
		return false
	}
}

func (m *mockSlotChecker) Acquire(ctx context.Context) error {
	select {
	case <-m.sem:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *mockSlotChecker) Release() {
	m.sem <- struct{}{}
}

func (m *mockSlotChecker) available() int {
	return len(m.sem)
}

// TestAcquirePreferBrain_BrainFree verifies that when preferBrain=true and
// Brain has a free slot, Brain is returned.
func TestAcquirePreferBrain_BrainFree(t *testing.T) {
	primarySlots := newMockSlotChecker(3)
	brainSlots := newMockSlotChecker(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots)

	choice := router.AcquireOrFallback(context.Background(), true)

	if choice.Model != "brain-model" {
		t.Fatalf("expected brain-model, got %s", choice.Model)
	}
	// Brain slot should be consumed (0 available).
	if brainSlots.available() != 0 {
		t.Fatalf("expected 0 brain slots available, got %d", brainSlots.available())
	}
	// Primary slots should be untouched.
	if primarySlots.available() != 3 {
		t.Fatalf("expected 3 primary slots available, got %d", primarySlots.available())
	}

	choice.Release()
	if brainSlots.available() != 1 {
		t.Fatalf("expected 1 brain slot after release, got %d", brainSlots.available())
	}
}

// TestAcquirePreferBrain_BrainBusy9BFree verifies that when preferBrain=true
// and Brain is busy but 9B has a slot, 9B is returned as fallback.
func TestAcquirePreferBrain_BrainBusy9BFree(t *testing.T) {
	primarySlots := newMockSlotChecker(3)
	brainSlots := newMockSlotChecker(1)

	// Exhaust Brain.
	brainSlots.TryAcquire()

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots)
	choice := router.AcquireOrFallback(context.Background(), true)

	if choice.Model != "primary-model" {
		t.Fatalf("expected primary-model fallback, got %s", choice.Model)
	}
	if primarySlots.available() != 2 {
		t.Fatalf("expected 2 primary slots available, got %d", primarySlots.available())
	}

	choice.Release()
	if primarySlots.available() != 3 {
		t.Fatalf("expected 3 primary slots after release, got %d", primarySlots.available())
	}
}

// TestAcquirePreferBrain_BothBusy verifies that when preferBrain=true and
// both are busy, it blocks on Brain until a slot is freed.
func TestAcquirePreferBrain_BothBusy(t *testing.T) {
	primarySlots := newMockSlotChecker(1)
	brainSlots := newMockSlotChecker(1)

	// Exhaust both.
	primarySlots.TryAcquire()
	brainSlots.TryAcquire()

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots)

	done := make(chan OrchestratorChoice, 1)
	go func() {
		choice := router.AcquireOrFallback(context.Background(), true)
		done <- choice
	}()

	// Should be blocking — verify no result yet.
	select {
	case <-done:
		t.Fatal("should not have returned yet")
	case <-time.After(50 * time.Millisecond):
		// expected
	}

	// Free Brain slot — should unblock.
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
	primarySlots := newMockSlotChecker(3)
	brainSlots := newMockSlotChecker(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots)
	choice := router.AcquireOrFallback(context.Background(), false)

	if choice.Model != "primary-model" {
		t.Fatalf("expected primary-model, got %s", choice.Model)
	}
	if primarySlots.available() != 2 {
		t.Fatalf("expected 2 primary slots available, got %d", primarySlots.available())
	}

	choice.Release()
}

// TestAcquirePrefer9B_PrimaryBusy verifies that preferBrain=false falls back
// to Brain when primary is busy.
func TestAcquirePrefer9B_PrimaryBusy(t *testing.T) {
	primarySlots := newMockSlotChecker(1)
	brainSlots := newMockSlotChecker(1)

	// Exhaust primary.
	primarySlots.TryAcquire()

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots)
	choice := router.AcquireOrFallback(context.Background(), false)

	if choice.Model != "brain-model" {
		t.Fatalf("expected brain-model fallback, got %s", choice.Model)
	}

	choice.Release()
}

// TestReacquire_SameSlotType verifies that Reacquire gets the same slot type
// that was originally acquired.
func TestReacquire_SameSlotType(t *testing.T) {
	primarySlots := newMockSlotChecker(3)
	brainSlots := newMockSlotChecker(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots)

	t.Run("brain_reacquire", func(t *testing.T) {
		choice := router.AcquireOrFallback(context.Background(), true)
		if choice.Model != "brain-model" {
			t.Fatalf("expected brain-model, got %s", choice.Model)
		}
		if brainSlots.available() != 0 {
			t.Fatalf("expected 0 brain slots, got %d", brainSlots.available())
		}

		// Release slot.
		choice.Release()
		if brainSlots.available() != 1 {
			t.Fatalf("expected 1 brain slot after release, got %d", brainSlots.available())
		}

		// Reacquire should get Brain slot back.
		if err := choice.Reacquire(context.Background()); err != nil {
			t.Fatalf("reacquire failed: %v", err)
		}
		if brainSlots.available() != 0 {
			t.Fatalf("expected 0 brain slots after reacquire, got %d", brainSlots.available())
		}

		choice.Release()
	})

	t.Run("primary_reacquire", func(t *testing.T) {
		choice := router.AcquireOrFallback(context.Background(), false)
		if choice.Model != "primary-model" {
			t.Fatalf("expected primary-model, got %s", choice.Model)
		}
		if primarySlots.available() != 2 {
			t.Fatalf("expected 2 primary slots, got %d", primarySlots.available())
		}

		// Release slot.
		choice.Release()
		if primarySlots.available() != 3 {
			t.Fatalf("expected 3 primary slots after release, got %d", primarySlots.available())
		}

		// Reacquire should get primary slot back.
		if err := choice.Reacquire(context.Background()); err != nil {
			t.Fatalf("reacquire failed: %v", err)
		}
		if primarySlots.available() != 2 {
			t.Fatalf("expected 2 primary slots after reacquire, got %d", primarySlots.available())
		}

		choice.Release()
	})
}

// TestPassthrough_IgnoresPreferBrain verifies that the passthrough router
// always returns the same client/model regardless of preferBrain.
func TestPassthrough_IgnoresPreferBrain(t *testing.T) {
	router := NewPassthroughRouter(nil, "passthrough-model")

	for _, preferBrain := range []bool{true, false} {
		choice := router.AcquireOrFallback(context.Background(), preferBrain)
		if choice.Model != "passthrough-model" {
			t.Fatalf("preferBrain=%v: expected passthrough-model, got %s", preferBrain, choice.Model)
		}
		// Release and Reacquire should be no-ops.
		choice.Release()
		if err := choice.Reacquire(context.Background()); err != nil {
			t.Fatalf("preferBrain=%v: reacquire failed: %v", preferBrain, err)
		}
	}
}

// TestReacquire_ContextCancelled verifies that Reacquire respects context
// cancellation.
func TestReacquire_ContextCancelled(t *testing.T) {
	primarySlots := newMockSlotChecker(1)
	brainSlots := newMockSlotChecker(1)

	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots)

	// Acquire a primary slot.
	choice := router.AcquireOrFallback(context.Background(), false)
	if choice.Model != "primary-model" {
		t.Fatalf("expected primary-model, got %s", choice.Model)
	}

	// Release it.
	choice.Release()

	// Exhaust primary so reacquire would block.
	primarySlots.TryAcquire()

	// Reacquire with cancelled context should fail.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := choice.Reacquire(ctx)
	if err == nil {
		t.Fatal("expected reacquire to fail with cancelled context")
	}
}

// TestConcurrentAcquireRelease verifies no races under concurrent access.
func TestConcurrentAcquireRelease(t *testing.T) {
	primarySlots := newMockSlotChecker(3)
	brainSlots := newMockSlotChecker(1)
	router := NewSlotRouter(nil, "primary-model", nil, "brain-model", primarySlots, brainSlots)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			choice := router.AcquireOrFallback(context.Background(), true)
			time.Sleep(time.Millisecond)
			choice.Release()
		}()
	}
	wg.Wait()

	// All slots should be returned.
	if primarySlots.available() != 3 {
		t.Fatalf("expected 3 primary slots, got %d", primarySlots.available())
	}
	if brainSlots.available() != 1 {
		t.Fatalf("expected 1 brain slot, got %d", brainSlots.available())
	}
}
