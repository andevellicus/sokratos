package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- Pure unit tests for query building ---

func TestBuildCheckQuery_WithTimestamp(t *testing.T) {
	// A known "last check" time should produce after:<unix>.
	since := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)

	query, effective := buildCheckQuery(since, now)

	expected := fmt.Sprintf("after:%d -in:trash -in:spam", since.Unix())
	if query != expected {
		t.Errorf("query = %q, want %q", query, expected)
	}
	if !effective.Equal(since) {
		t.Errorf("effectiveSince = %v, want %v", effective, since)
	}
}

func TestBuildCheckQuery_ZeroTimeFallsBackTo1h(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	query, effective := buildCheckQuery(time.Time{}, now)

	expectedSince := now.Add(-1 * time.Hour)
	expectedQuery := fmt.Sprintf("after:%d -in:trash -in:spam", expectedSince.Unix())
	if query != expectedQuery {
		t.Errorf("query = %q, want %q", query, expectedQuery)
	}
	if !effective.Equal(expectedSince) {
		t.Errorf("effectiveSince = %v, want %v", effective, expectedSince)
	}
}

func TestBuildCheckQuery_ExcludesTrashAndSpam(t *testing.T) {
	now := time.Now()
	query, _ := buildCheckQuery(now.Add(-5*time.Minute), now)

	if !strings.Contains(query, "-in:trash") {
		t.Error("query missing -in:trash")
	}
	if !strings.Contains(query, "-in:spam") {
		t.Error("query missing -in:spam")
	}
}

func TestBuildCheckQuery_UsesUnixEpoch(t *testing.T) {
	since := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)

	query, _ := buildCheckQuery(since, now)

	prefix := fmt.Sprintf("after:%d", since.Unix())
	if !strings.HasPrefix(query, prefix) {
		t.Errorf("query %q does not start with %q", query, prefix)
	}
}

func TestBuildCheckQuery_NarrowInterval(t *testing.T) {
	// 5-minute routine: since should be ~5m ago.
	now := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)
	since := now.Add(-5 * time.Minute)

	query, _ := buildCheckQuery(since, now)

	expected := fmt.Sprintf("after:%d -in:trash -in:spam", since.Unix())
	if query != expected {
		t.Errorf("query = %q, want %q", query, expected)
	}
}

func TestBuildCheckQuery_WideInterval(t *testing.T) {
	// 6-hour routine: since should be ~6h ago, query should use that timestamp.
	now := time.Date(2026, 3, 1, 18, 0, 0, 0, time.UTC)
	since := now.Add(-6 * time.Hour)

	query, _ := buildCheckQuery(since, now)

	expected := fmt.Sprintf("after:%d -in:trash -in:spam", since.Unix())
	if query != expected {
		t.Errorf("query = %q, want %q", query, expected)
	}
}

// --- Integration tests for meta table (require DATABASE_URL) ---

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set, skipping DB integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Ensure the meta table exists.
	_, err = pool.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS processed_emails_meta (
			key VARCHAR(64) PRIMARY KEY,
			checked_at TIMESTAMPTZ NOT NULL
		)`)
	if err != nil {
		t.Fatalf("failed to create meta table: %v", err)
	}

	// Clean slate.
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM processed_emails_meta WHERE key = 'last_check'`)
	return pool
}

func TestLastEmailCheck_NoRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	got := lastEmailCheck(ctx, pool)
	if !got.IsZero() {
		t.Errorf("expected zero time for missing row, got %v", got)
	}
}

func TestSaveAndLoadEmailCheck(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ts := time.Now().Truncate(time.Microsecond) // pg truncates to µs
	saveEmailCheck(ctx, pool, ts)

	got := lastEmailCheck(ctx, pool)
	if !got.Truncate(time.Microsecond).Equal(ts) {
		t.Errorf("round-trip failed: saved %v, got %v", ts, got)
	}
}

func TestSaveEmailCheck_Upsert(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	first := time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond)
	second := time.Now().Truncate(time.Microsecond)

	saveEmailCheck(ctx, pool, first)
	saveEmailCheck(ctx, pool, second)

	got := lastEmailCheck(ctx, pool)
	if !got.Truncate(time.Microsecond).Equal(second) {
		t.Errorf("upsert failed: expected %v, got %v", second, got)
	}
}

func TestLastEmailCheck_NilPool(t *testing.T) {
	got := lastEmailCheck(context.Background(), nil)
	if !got.IsZero() {
		t.Errorf("expected zero time for nil pool, got %v", got)
	}
}

func TestSaveEmailCheck_NilPool(t *testing.T) {
	// Should not panic.
	saveEmailCheck(context.Background(), nil, time.Now())
}
