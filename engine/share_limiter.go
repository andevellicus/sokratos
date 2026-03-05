package engine

import (
	"sync"
	"time"
)

// ShareLimiter rate-limits proactive user notifications.
type ShareLimiter struct {
	mu          sync.Mutex
	daily       int       // shares sent today
	lastSent    time.Time // when the last share was sent
	dayStart    time.Time // start of current day
	maxDaily    int       // max shares per day (default 3)
	minInterval time.Duration // min gap between shares (default 30min)
}

// NewShareLimiter creates a limiter with the given daily cap.
func NewShareLimiter(maxDaily int) *ShareLimiter {
	if maxDaily <= 0 {
		maxDaily = 3
	}
	now := time.Now()
	return &ShareLimiter{
		maxDaily:    maxDaily,
		minInterval: 30 * time.Minute,
		dayStart:    startOfDay(now),
	}
}

// Allow returns true if a proactive share is allowed.
func (sl *ShareLimiter) Allow() bool {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	now := time.Now()

	// Reset daily counter at midnight.
	today := startOfDay(now)
	if today.After(sl.dayStart) {
		sl.daily = 0
		sl.dayStart = today
	}

	if sl.daily >= sl.maxDaily {
		return false
	}
	if !sl.lastSent.IsZero() && now.Sub(sl.lastSent) < sl.minInterval {
		return false
	}

	sl.daily++
	sl.lastSent = now
	return true
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
