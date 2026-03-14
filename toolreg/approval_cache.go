package toolreg

import (
	"sync"
	"time"
)

// ApprovalCache is a thread-safe time-limited approval cache.
type ApprovalCache struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
}

// NewApprovalCache returns a cache that auto-expires entries after ttl.
func NewApprovalCache(ttl time.Duration) *ApprovalCache {
	return &ApprovalCache{
		m:   make(map[string]time.Time),
		ttl: ttl,
	}
}

// Check returns true if key was recorded within the TTL window.
func (c *ApprovalCache) Check(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[key]
	return ok && time.Since(t) < c.ttl
}

// Record stores an approval timestamp for key.
func (c *ApprovalCache) Record(key string) {
	c.mu.Lock()
	c.m[key] = time.Now()
	c.mu.Unlock()
}
