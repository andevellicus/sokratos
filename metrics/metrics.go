package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"sokratos/logger"
)

// Event represents a single metric data point.
type Event struct {
	Ts    time.Time
	Name  string
	Value float64
	Dims  map[string]string
}

const (
	bufferCap      = 256
	flushThreshold = 64
	flushInterval  = 10 * time.Second
)

// Collector buffers metric events and flushes them to PostgreSQL in batches.
// All public methods are safe for concurrent use and never block the caller.
// A nil Collector or one with a nil pool is a no-op.
type Collector struct {
	pool    *pgxpool.Pool
	mu      sync.Mutex
	buf     []Event
	flushCh chan struct{}
	stopCh  chan struct{}
	done    chan struct{}
}

// New creates a Collector backed by the given pool.
// If pool is nil, all methods become no-ops.
func New(pool *pgxpool.Pool) *Collector {
	c := &Collector{
		pool:    pool,
		buf:     make([]Event, 0, bufferCap),
		flushCh: make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go c.flusher()
	return c
}

// Emit records a metric event with the given name, value, and dimensions.
func (c *Collector) Emit(name string, value float64, dims map[string]string) {
	if c == nil || c.pool == nil {
		return
	}
	ev := Event{Ts: time.Now(), Name: name, Value: value, Dims: dims}
	c.mu.Lock()
	if len(c.buf) >= bufferCap {
		// Drop oldest to make room.
		copy(c.buf, c.buf[1:])
		c.buf = c.buf[:bufferCap-1]
	}
	c.buf = append(c.buf, ev)
	shouldSignal := len(c.buf) >= flushThreshold
	c.mu.Unlock()

	if shouldSignal {
		select {
		case c.flushCh <- struct{}{}:
		default:
		}
	}
}

// EmitDuration records a duration metric in milliseconds.
func (c *Collector) EmitDuration(name string, d time.Duration, dims map[string]string) {
	c.Emit(name, float64(d.Milliseconds()), dims)
}

// Since is a convenience for EmitDuration(name, time.Since(start), dims).
func (c *Collector) Since(name string, start time.Time, dims map[string]string) {
	c.EmitDuration(name, time.Since(start), dims)
}

// Close signals the flusher to stop and waits for it to finish.
func (c *Collector) Close() {
	if c == nil {
		return
	}
	close(c.stopCh)
	<-c.done
}

func (c *Collector) flusher() {
	defer close(c.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.flush()
		case <-c.flushCh:
			c.flush()
		case <-c.stopCh:
			c.flush() // drain remaining
			return
		}
	}
}

func (c *Collector) flush() {
	c.mu.Lock()
	if len(c.buf) == 0 {
		c.mu.Unlock()
		return
	}
	batch := c.buf
	c.buf = make([]Event, 0, bufferCap)
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.insertBatch(ctx, batch); err != nil {
		logger.Log.Warnf("metrics flush failed (%d events dropped): %v", len(batch), err)
	}
}

func (c *Collector) insertBatch(ctx context.Context, batch []Event) error {
	if len(batch) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("INSERT INTO metrics (ts, name, value, dims) VALUES ")

	args := make([]any, 0, len(batch)*4)
	for i, ev := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*4 + 1
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d)", base, base+1, base+2, base+3)

		var dimsJSON []byte
		if len(ev.Dims) > 0 {
			dimsJSON, _ = json.Marshal(ev.Dims)
		}
		args = append(args, ev.Ts, ev.Name, ev.Value, dimsJSON)
	}

	_, err := c.pool.Exec(ctx, sb.String(), args...)
	return err
}

// BufLen returns the current buffer length (for testing).
func (c *Collector) BufLen() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buf)
}
