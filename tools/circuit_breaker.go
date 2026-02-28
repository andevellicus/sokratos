package tools

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"sokratos/logger"
)

const (
	cbFailThreshold = 3              // consecutive failures before opening
	cbOpenDuration  = 2 * time.Minute // initial open duration
	cbMaxOpen       = 10 * time.Minute // cap on exponential backoff
)

// circuitState represents the three states of a circuit breaker.
type circuitState int

const (
	circuitClosed   circuitState = iota // normal operation
	circuitOpen                         // failing fast
	circuitHalfOpen                     // allowing a single probe
)

// circuitBreaker implements a simple circuit breaker with exponential backoff.
// After cbFailThreshold consecutive failures the breaker opens for cbOpenDuration,
// doubling the open window on each subsequent failure up to cbMaxOpen.
// In half-open state a single request is allowed through as a probe.
type circuitBreaker struct {
	mu          sync.Mutex
	name        string
	state       circuitState
	failCount   int
	openUntil   time.Time
	openDur     time.Duration // current backoff duration
}

func newCircuitBreaker(name string) circuitBreaker {
	return circuitBreaker{
		name:    name,
		state:   circuitClosed,
		openDur: cbOpenDuration,
	}
}

// check returns an error if the circuit is open (fail-fast). If the open
// duration has elapsed, it transitions to half-open and allows one probe.
func (cb *circuitBreaker) check() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case circuitClosed:
		return nil
	case circuitOpen:
		if time.Now().After(cb.openUntil) {
			cb.state = circuitHalfOpen
			logger.Log.Infof("[circuit:%s] half-open, allowing probe", cb.name)
			return nil
		}
		return fmt.Errorf("circuit breaker %s is open (retry after %s)", cb.name, time.Until(cb.openUntil).Truncate(time.Second))
	case circuitHalfOpen:
		// Already probing — block additional requests until the probe resolves.
		return fmt.Errorf("circuit breaker %s is half-open, probe in progress", cb.name)
	}
	return nil
}

// recordSuccess resets the breaker to closed state.
func (cb *circuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state != circuitClosed {
		logger.Log.Infof("[circuit:%s] closed (success)", cb.name)
	}
	cb.failCount = 0
	cb.state = circuitClosed
	cb.openDur = cbOpenDuration
}

// recordFailureIfServer records a failure only if err represents a genuine
// server problem (5xx, connection refused, etc.). Context cancellations and
// deadline exceeded errors are the caller's fault (tight timeout, cancelled
// parent), not a server fault, and should NOT trip the breaker.
func (cb *circuitBreaker) recordFailureIfServer(err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return
	}
	cb.recordFailure()
}

// recordFailure increments the failure counter and opens the breaker if the
// threshold is reached. Uses exponential backoff for the open duration.
func (cb *circuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failCount++
	if cb.failCount >= cbFailThreshold {
		cb.state = circuitOpen
		cb.openUntil = time.Now().Add(cb.openDur)
		logger.Log.Warnf("[circuit:%s] opened for %s (failures: %d)", cb.name, cb.openDur, cb.failCount)
		// Exponential backoff: double the open duration, cap at cbMaxOpen.
		cb.openDur *= 2
		if cb.openDur > cbMaxOpen {
			cb.openDur = cbMaxOpen
		}
	}
}
