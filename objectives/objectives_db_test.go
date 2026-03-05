package objectives

import (
	"context"
	"strings"
	"testing"

	"sokratos/internal/testutil"
)

func TestObjectiveCRUD(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	// Create
	id, err := Create(ctx, pool, "Test objective for CRUD", "high", "explicit")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM objectives WHERE id = $1", id)
	})

	// Get
	obj, err := Get(ctx, pool, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if obj.Summary != "Test objective for CRUD" {
		t.Errorf("Summary = %q, want %q", obj.Summary, "Test objective for CRUD")
	}
	if obj.Status != "active" {
		t.Errorf("Status = %q, want %q", obj.Status, "active")
	}
	if obj.Priority != "high" {
		t.Errorf("Priority = %q, want %q", obj.Priority, "high")
	}

	// AppendProgress twice, check separator
	if err := AppendProgress(ctx, pool, id, "Step 1 done"); err != nil {
		t.Fatalf("AppendProgress #1: %v", err)
	}
	if err := AppendProgress(ctx, pool, id, "Step 2 done"); err != nil {
		t.Fatalf("AppendProgress #2: %v", err)
	}
	obj, _ = Get(ctx, pool, id)
	if !strings.Contains(obj.ProgressNotes, "---") {
		t.Errorf("ProgressNotes missing separator: %q", obj.ProgressNotes)
	}
	if !strings.Contains(obj.ProgressNotes, "Step 1 done") || !strings.Contains(obj.ProgressNotes, "Step 2 done") {
		t.Errorf("ProgressNotes missing content: %q", obj.ProgressNotes)
	}

	// IncrementAttempts
	if err := IncrementAttempts(ctx, pool, id); err != nil {
		t.Fatalf("IncrementAttempts: %v", err)
	}
	obj, _ = Get(ctx, pool, id)
	if obj.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", obj.Attempts)
	}
	if obj.LastPursued == nil {
		t.Error("LastPursued should be set after IncrementAttempts")
	}

	// UpdateStatus
	if err := UpdateStatus(ctx, pool, id, "paused"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	obj, _ = Get(ctx, pool, id)
	if obj.Status != "paused" {
		t.Errorf("Status = %q, want %q", obj.Status, "paused")
	}

	// UpdatePriority
	if err := UpdatePriority(ctx, pool, id, "low"); err != nil {
		t.Fatalf("UpdatePriority: %v", err)
	}
	obj, _ = Get(ctx, pool, id)
	if obj.Priority != "low" {
		t.Errorf("Priority = %q, want %q", obj.Priority, "low")
	}

	// ListActive — paused should not appear
	if err := UpdateStatus(ctx, pool, id, "active"); err != nil {
		t.Fatalf("UpdateStatus back to active: %v", err)
	}
	active, err := ListActive(ctx, pool)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	found := false
	for _, o := range active {
		if o.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListActive should include the test objective")
	}

	// Complete
	if err := Complete(ctx, pool, id); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	obj, _ = Get(ctx, pool, id)
	if obj.Status != "completed" {
		t.Errorf("Status after Complete = %q, want %q", obj.Status, "completed")
	}

	// ListAll — completed should appear
	all, err := ListAll(ctx, pool)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	found = false
	for _, o := range all {
		if o.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListAll should include completed objective")
	}

	// Retire
	if err := Retire(ctx, pool, id); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	obj, _ = Get(ctx, pool, id)
	if obj.Status != "retired" {
		t.Errorf("Status after Retire = %q, want %q", obj.Status, "retired")
	}
}

func TestFindSimilar(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	id, err := Create(ctx, pool, "Learn advanced Go concurrency patterns", "medium", "explicit")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM objectives WHERE id = $1", id)
	})

	// Should find by prefix.
	similar, err := FindSimilar(ctx, pool, "Learn advanced Go")
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	found := false
	for _, o := range similar {
		if o.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Error("FindSimilar should find the test objective by prefix")
	}

	// Should not find unrelated.
	unrelated, err := FindSimilar(ctx, pool, "Deploy Kubernetes cluster")
	if err != nil {
		t.Fatalf("FindSimilar unrelated: %v", err)
	}
	for _, o := range unrelated {
		if o.ID == id {
			t.Error("FindSimilar should not match unrelated query")
		}
	}
}
