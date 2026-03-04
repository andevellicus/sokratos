package clients

import (
	"context"
	"fmt"
	"time"

	"sokratos/logger"
	"sokratos/memory"
)

// lowPriorityRetryDelay is how long to wait before re-queuing a low-priority
// item that was admission-dropped under queue pressure. An hour is long enough
// for any burst to drain while still eventually completing the work.
const lowPriorityRetryDelay = 1 * time.Hour

// WorkExecFunc executes a single work request and returns the result.
type WorkExecFunc func(ctx context.Context, req memory.WorkRequest) (string, error)

// WorkQueue manages a buffered channel of background LLM tasks with retry
// support. Items are processed by worker goroutines using the provided
// exec function, with fresh contexts per item so queue wait time doesn't
// eat into inference time.
type WorkQueue struct {
	ch     chan memory.WorkRequest
	logTag string
}

// NewWorkQueue creates a work queue with the given capacity and starts worker
// goroutines. execFn is called for each item to perform the actual LLM work.
func NewWorkQueue(capacity, workers int, logTag string, execFn WorkExecFunc) *WorkQueue {
	wq := &WorkQueue{
		ch:     make(chan memory.WorkRequest, capacity),
		logTag: logTag,
	}
	for i := 0; i < workers; i++ {
		go wq.process(execFn)
	}
	return wq
}

// QueueWork submits a background LLM task. Items are processed as worker
// goroutines become available. Returns immediately; drops the item if the
// queue buffer is full. Low-priority items are also dropped when the queue
// is ≥75% full (admission control to prevent HOL blocking).
func (wq *WorkQueue) QueueWork(item memory.WorkRequest) {
	// Admission control: drop low-priority items when queue is under pressure.
	// This prevents background enrichment tasks from filling the queue ahead
	// of critical distillation work.
	pri := item.Priority
	if pri == 0 {
		pri = memory.PriorityNormal
	}
	depth := len(wq.ch)
	threshold := cap(wq.ch) * 3 / 4
	if depth >= threshold && pri <= memory.PriorityLow {
		logger.Log.Warnf("%s queue under pressure (depth=%d/%d), deferring low-priority: %s",
			wq.logTag, depth, cap(wq.ch), item.Label)
		// Re-queue at normal priority after an hour so bursts don't
		// permanently discard background work.
		item.Priority = memory.PriorityNormal
		go func() {
			time.Sleep(lowPriorityRetryDelay)
			wq.QueueWork(item)
		}()
		return
	}

	select {
	case wq.ch <- item:
		logger.Log.Debugf("%s queued: %s (depth=%d/%d)", wq.logTag, item.Label, len(wq.ch), cap(wq.ch))
	default:
		logger.Log.Warnf("%s work queue full (cap=%d), dropping: %s", wq.logTag, cap(wq.ch), item.Label)
		if item.OnComplete != nil {
			item.OnComplete("", fmt.Errorf("work queue full"))
		}
	}
}

// process drains the work channel, executing items via execFn with fresh
// contexts. On transient failure, items with Retries > 0 are requeued after
// a brief backoff.
func (wq *WorkQueue) process(execFn WorkExecFunc) {
	for item := range wq.ch {
		ctx, cancel := context.WithTimeout(context.Background(), item.Timeout)
		result, err := execFn(ctx, item)
		cancel()

		if err != nil && item.Retries > 0 {
			item.Retries--
			backoff := 2 * time.Second
			logger.Log.Warnf("%s %s failed (%v), retrying in %v (%d left)",
				wq.logTag, item.Label, err, backoff, item.Retries)
			time.Sleep(backoff)
			select {
			case wq.ch <- item:
				continue
			default:
				logger.Log.Warnf("%s %s retry failed: queue full, delivering error", wq.logTag, item.Label)
			}
		}

		if item.OnComplete != nil {
			item.OnComplete(result, err)
		}
	}
}
