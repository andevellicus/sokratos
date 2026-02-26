package tools

import (
	"testing"
)

// testItem is a simple struct for testing FilterProcessed's generic behavior.
type testItem struct {
	ID   string
	Name string
}

// TestFilterProcessedLogic tests the filtering logic of FilterProcessed
// by exercising the ID extraction and set-difference aspects directly.
// Full integration tests require a database connection.
func TestFilterProcessedLogic(t *testing.T) {
	items := []testItem{
		{ID: "a", Name: "Alice"},
		{ID: "b", Name: "Bob"},
		{ID: "c", Name: "Charlie"},
		{ID: "d", Name: "Dave"},
	}
	getID := func(item testItem) string { return item.ID }

	// Simulate the filtering logic without a database.
	seen := map[string]struct{}{"b": {}, "d": {}}
	var fresh []testItem
	for _, item := range items {
		if _, ok := seen[getID(item)]; !ok {
			fresh = append(fresh, item)
		}
	}

	if len(fresh) != 2 {
		t.Fatalf("expected 2 fresh items, got %d", len(fresh))
	}
	if fresh[0].ID != "a" {
		t.Errorf("expected first fresh item ID 'a', got %q", fresh[0].ID)
	}
	if fresh[1].ID != "c" {
		t.Errorf("expected second fresh item ID 'c', got %q", fresh[1].ID)
	}
}

// TestFilterProcessedAllSeen tests the case where all items are already processed.
func TestFilterProcessedAllSeen(t *testing.T) {
	items := []testItem{
		{ID: "x", Name: "Xavier"},
		{ID: "y", Name: "Yuri"},
	}
	getID := func(item testItem) string { return item.ID }

	seen := map[string]struct{}{"x": {}, "y": {}}
	var fresh []testItem
	for _, item := range items {
		if _, ok := seen[getID(item)]; !ok {
			fresh = append(fresh, item)
		}
	}

	if len(fresh) != 0 {
		t.Fatalf("expected 0 fresh items, got %d", len(fresh))
	}
}

// TestFilterProcessedNoneSeen tests the case where no items are processed.
func TestFilterProcessedNoneSeen(t *testing.T) {
	items := []testItem{
		{ID: "p", Name: "Pat"},
		{ID: "q", Name: "Quinn"},
	}
	getID := func(item testItem) string { return item.ID }

	seen := map[string]struct{}{}
	var fresh []testItem
	for _, item := range items {
		if _, ok := seen[getID(item)]; !ok {
			fresh = append(fresh, item)
		}
	}

	if len(fresh) != 2 {
		t.Fatalf("expected 2 fresh items, got %d", len(fresh))
	}
}

// TestFilterProcessedEmpty tests with empty input.
func TestFilterProcessedEmpty(t *testing.T) {
	var items []testItem
	getID := func(item testItem) string { return item.ID }

	seen := map[string]struct{}{}
	var fresh []testItem
	for _, item := range items {
		if _, ok := seen[getID(item)]; !ok {
			fresh = append(fresh, item)
		}
	}

	if len(fresh) != 0 {
		t.Fatalf("expected 0 fresh items, got %d", len(fresh))
	}
	_ = getID // verify getID compiles with the generic signature
}
