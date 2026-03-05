package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShareLimiter_FirstAllow(t *testing.T) {
	sl := NewShareLimiter(3)
	if !sl.Allow() {
		t.Error("first Allow() should return true")
	}
}

func TestShareLimiter_DailyCap(t *testing.T) {
	sl := NewShareLimiter(2)
	// Disable min interval by backdating lastSent.
	sl.mu.Lock()
	sl.minInterval = 0
	sl.mu.Unlock()

	if !sl.Allow() {
		t.Error("Allow() #1 should return true")
	}
	if !sl.Allow() {
		t.Error("Allow() #2 should return true")
	}
	if sl.Allow() {
		t.Error("Allow() #3 should return false (daily cap reached)")
	}
}

func TestShareLimiter_MinInterval(t *testing.T) {
	sl := NewShareLimiter(10) // high cap so it doesn't interfere
	if !sl.Allow() {
		t.Fatal("first Allow() should return true")
	}
	// Second call within 30min should be denied.
	if sl.Allow() {
		t.Error("Allow() within minInterval should return false")
	}
}

func TestShareLimiter_MidnightReset(t *testing.T) {
	sl := NewShareLimiter(1)
	sl.mu.Lock()
	sl.minInterval = 0
	sl.mu.Unlock()

	if !sl.Allow() {
		t.Fatal("first Allow() should return true")
	}
	if sl.Allow() {
		t.Fatal("second Allow() should return false (cap=1)")
	}

	// Simulate midnight reset by backdating dayStart to yesterday.
	sl.mu.Lock()
	sl.dayStart = sl.dayStart.AddDate(0, 0, -1)
	sl.lastSent = time.Time{} // clear interval constraint
	sl.mu.Unlock()

	if !sl.Allow() {
		t.Error("Allow() after midnight reset should return true")
	}
}

func TestShareLimiter_DefaultMax(t *testing.T) {
	sl := NewShareLimiter(0)
	if sl.maxDaily != 3 {
		t.Errorf("NewShareLimiter(0).maxDaily = %d, want 3", sl.maxDaily)
	}
}

func TestShareLimiter_Concurrent(t *testing.T) {
	sl := NewShareLimiter(3)
	sl.mu.Lock()
	sl.minInterval = 0
	sl.mu.Unlock()

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sl.Allow() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != 3 {
		t.Errorf("concurrent Allow(): got %d trues, want 3", got)
	}
}

func TestStartOfDay(t *testing.T) {
	afternoon := time.Date(2026, 3, 4, 14, 30, 45, 123, time.Local)
	got := startOfDay(afternoon)
	want := time.Date(2026, 3, 4, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("startOfDay(afternoon) = %v, want %v", got, want)
	}

	midnight := time.Date(2026, 3, 4, 0, 0, 0, 0, time.Local)
	got2 := startOfDay(midnight)
	if !got2.Equal(midnight) {
		t.Errorf("startOfDay(midnight) = %v, want %v", got2, midnight)
	}
}
