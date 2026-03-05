package adaptive

import (
	"context"
	"testing"

	"sokratos/internal/testutil"
)

func TestClamp(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value float64
		want  float64
	}{
		{"known key within range", "triage_conversation_threshold", 4.0, 4.0},
		{"known key below min", "triage_conversation_threshold", 0.5, 1.0},
		{"known key above max", "triage_conversation_threshold", 10.0, 8.0},
		{"known key at min boundary", "triage_conversation_threshold", 1.0, 1.0},
		{"known key at max boundary", "triage_conversation_threshold", 8.0, 8.0},
		{"unknown key passthrough", "unknown_param", 42.0, 42.0},
		{"curiosity within range", "curiosity_cooldown_hours", 3.0, 3.0},
		{"curiosity below min", "curiosity_cooldown_hours", 0.1, 0.5},
		{"curiosity above max", "curiosity_cooldown_hours", 10.0, 6.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Clamp(tt.key, tt.value)
			if got != tt.want {
				t.Errorf("Clamp(%q, %v) = %v, want %v", tt.key, tt.value, got, tt.want)
			}
		})
	}
}

func TestIsValidKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"triage_conversation_threshold", true},
		{"triage_conversation_unverified_threshold", true},
		{"triage_email_threshold", true},
		{"curiosity_cooldown_hours", true},
		{"unknown_key", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := IsValidKey(tt.key)
			if got != tt.want {
				t.Errorf("IsValidKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestGetSetIntegration(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM adaptive_params WHERE key LIKE 'test_%'")
	})

	// Get non-existent key returns default.
	got := Get(ctx, pool, "test_nonexistent", 99.0)
	if got != 99.0 {
		t.Errorf("Get non-existent = %v, want 99.0", got)
	}

	// Set a key, then Get returns set value.
	if err := Set(ctx, pool, "test_param", 3.14, "test", "unit test"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got = Get(ctx, pool, "test_param", 0)
	if got != 3.14 {
		t.Errorf("Get after Set = %v, want 3.14", got)
	}

	// Upsert: Set same key with different value.
	if err := Set(ctx, pool, "test_param", 2.72, "test", "updated"); err != nil {
		t.Fatalf("Set upsert: %v", err)
	}
	got = Get(ctx, pool, "test_param", 0)
	if got != 2.72 {
		t.Errorf("Get after upsert = %v, want 2.72", got)
	}
}

func TestGetAllIntegration(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM adaptive_params WHERE key LIKE 'test_%'")
	})

	// Insert two test params.
	if err := Set(ctx, pool, "test_all_a", 1.0, "test", "a"); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if err := Set(ctx, pool, "test_all_b", 2.0, "test", "b"); err != nil {
		t.Fatalf("Set b: %v", err)
	}

	params := GetAll(ctx, pool)
	found := map[string]bool{}
	for _, p := range params {
		found[p.Key] = true
	}
	if !found["test_all_a"] || !found["test_all_b"] {
		t.Errorf("GetAll missing test params, got keys: %v", found)
	}
}

func TestNilPool(t *testing.T) {
	ctx := context.Background()

	got := Get(ctx, nil, "any_key", 42.0)
	if got != 42.0 {
		t.Errorf("Get(nil pool) = %v, want 42.0", got)
	}

	if err := Set(ctx, nil, "any_key", 1.0, "test", "test"); err != nil {
		t.Errorf("Set(nil pool) = %v, want nil", err)
	}

	params := GetAll(ctx, nil)
	if params != nil {
		t.Errorf("GetAll(nil pool) = %v, want nil", params)
	}
}
