package metrics

import (
	"os"
	"testing"
	"time"

	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNilCollectorNoOp(t *testing.T) {
	var c *Collector
	// Must not panic.
	c.Emit("test", 1, nil)
	c.EmitDuration("test", time.Second, nil)
	c.Since("test", time.Now(), nil)
	c.Close()
	if c.BufLen() != 0 {
		t.Fatal("expected 0")
	}
}

func TestNilPoolNoOp(t *testing.T) {
	c := New(nil)
	defer c.Close()
	c.Emit("test", 1, nil)
	c.EmitDuration("test", time.Second, nil)
	c.Since("test", time.Now(), nil)
	if c.BufLen() != 0 {
		t.Fatal("nil pool should not buffer events")
	}
}

func TestBufferOverflowDropsOldest(t *testing.T) {
	// Use a collector with nil pool so flush is a no-op but Emit still buffers.
	// We need a non-nil pool to actually buffer, so we use a trick:
	// create a collector manually with a non-nil pool placeholder.
	c := &Collector{
		pool:    &pgxpool.Pool{}, // non-nil but unusable — flush will fail (and log warning)
		buf:     make([]Event, 0, bufferCap),
		flushCh: make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	// Don't start the flusher — we test buffer behavior only.
	go func() { <-c.stopCh; close(c.done) }()
	defer c.Close()

	// Fill to capacity.
	for i := range bufferCap {
		c.Emit("fill", float64(i), nil)
	}
	if c.BufLen() != bufferCap {
		t.Fatalf("expected %d, got %d", bufferCap, c.BufLen())
	}

	// One more should drop the oldest.
	c.Emit("overflow", 999, nil)
	if c.BufLen() != bufferCap {
		t.Fatalf("expected %d after overflow, got %d", bufferCap, c.BufLen())
	}

	c.mu.Lock()
	first := c.buf[0]
	last := c.buf[len(c.buf)-1]
	c.mu.Unlock()

	// Oldest (value=0) should be gone; first should be value=1.
	if first.Value != 1 {
		t.Fatalf("expected oldest to be dropped, first value = %v", first.Value)
	}
	if last.Name != "overflow" || last.Value != 999 {
		t.Fatalf("expected overflow event at end, got %v", last)
	}
}

func TestEmitDurationConvertsToMs(t *testing.T) {
	c := &Collector{
		pool:    &pgxpool.Pool{},
		buf:     make([]Event, 0, bufferCap),
		flushCh: make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() { <-c.stopCh; close(c.done) }()
	defer c.Close()

	c.EmitDuration("test", 1500*time.Millisecond, map[string]string{"k": "v"})
	c.mu.Lock()
	ev := c.buf[0]
	c.mu.Unlock()

	if ev.Value != 1500 {
		t.Fatalf("expected 1500ms, got %v", ev.Value)
	}
	if ev.Dims["k"] != "v" {
		t.Fatal("dims not preserved")
	}
}

func TestIntegrationFlush(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Ensure table exists.
	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS metrics (
		id BIGSERIAL PRIMARY KEY,
		ts TIMESTAMPTZ NOT NULL DEFAULT now(),
		name VARCHAR(80) NOT NULL,
		value FLOAT NOT NULL DEFAULT 1,
		dims JSONB
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Clean up test data.
	defer pool.Exec(ctx, `DELETE FROM metrics WHERE name LIKE 'test.%'`)

	c := New(pool)
	defer c.Close()

	c.Emit("test.counter", 1, map[string]string{"env": "test"})
	c.EmitDuration("test.latency", 42*time.Millisecond, nil)

	// Close triggers final flush.
	c.Close()

	var count int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM metrics WHERE name LIKE 'test.%'`).Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 flushed events, got %d", count)
	}
}
