package tools

import (
	"sync"
	"time"

	"sokratos/logger"
)

// RetryQueue is a bounded work queue that retries failed tasks periodically.
// Use it for any background work that should be deferred rather than dropped
// when a backend is temporarily unavailable (circuit breaker open, timeout,
// slot contention, etc.).
type RetryQueue struct {
	mu         sync.Mutex
	items      []retryItem
	name       string
	maxItems   int
	maxRetries int
	interval   time.Duration
	stopCh     chan struct{}
}

type retryItem struct {
	fn       func() error
	label    string
	retries  int
	addedAt  time.Time
}

// RetryQueueConfig holds configuration for a RetryQueue.
type RetryQueueConfig struct {
	Name       string        // log tag (e.g. "triage", "distillation")
	MaxItems   int           // max buffered items (default 50)
	MaxRetries int           // max retries per item (default 3)
	Interval   time.Duration // retry interval (default 30s)
}

// NewRetryQueue creates a bounded retry queue with the given config.
func NewRetryQueue(cfg RetryQueueConfig) *RetryQueue {
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = 50
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	return &RetryQueue{
		name:       cfg.Name,
		maxItems:   cfg.MaxItems,
		maxRetries: cfg.MaxRetries,
		interval:   cfg.Interval,
		stopCh:     make(chan struct{}),
	}
}

// Start begins the background retry loop. Call once at startup.
func (q *RetryQueue) Start() {
	go q.loop()
}

// Stop signals the retry loop to exit.
func (q *RetryQueue) Stop() {
	close(q.stopCh)
}

// Enqueue adds a retryable work item to the queue. fn will be called on each
// retry attempt; return nil to mark success, or an error to retry later.
// label is used for logging (e.g. "conversation triage: user asked about...").
func (q *RetryQueue) Enqueue(label string, fn func() error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) >= q.maxItems {
		logger.Log.Warnf("[retry:%s] queue full (%d), dropping oldest item", q.name, q.maxItems)
		q.items = q.items[1:]
	}

	q.items = append(q.items, retryItem{
		fn:      fn,
		label:   label,
		addedAt: time.Now(),
	})
	logger.Log.Infof("[retry:%s] enqueued: %s (depth: %d)", q.name, label, len(q.items))
}

// Len returns the current queue depth.
func (q *RetryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *RetryQueue) loop() {
	ticker := time.NewTicker(q.interval)
	defer ticker.Stop()

	for {
		select {
		case <-q.stopCh:
			return
		case <-ticker.C:
			q.drain()
		}
	}
}

func (q *RetryQueue) drain() {
	q.mu.Lock()
	if len(q.items) == 0 {
		q.mu.Unlock()
		return
	}
	batch := q.items
	q.items = nil
	q.mu.Unlock()

	logger.Log.Infof("[retry:%s] draining %d items", q.name, len(batch))

	var requeue []retryItem
	for i := range batch {
		item := &batch[i]
		if err := item.fn(); err != nil {
			item.retries++
			if item.retries >= q.maxRetries {
				logger.Log.Warnf("[retry:%s] dropped after %d retries: %s (%v)", q.name, item.retries, item.label, err)
				continue
			}
			logger.Log.Debugf("[retry:%s] will retry (%d/%d): %s", q.name, item.retries, q.maxRetries, item.label)
			requeue = append(requeue, *item)
		} else {
			logger.Log.Infof("[retry:%s] succeeded: %s", q.name, item.label)
		}
	}

	if len(requeue) > 0 {
		q.mu.Lock()
		q.items = append(requeue, q.items...)
		q.mu.Unlock()
	}
}
