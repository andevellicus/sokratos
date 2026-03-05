package memory

import (
	"context"
	"os"
	"strings"
	"testing"

	"sokratos/internal/testutil"
	"sokratos/logger"
)

func init() {
	// Personality functions use logger.Log.
	_ = logger.Init(os.TempDir())
}

func TestPersonalityUpsertAndGet(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	// Insert
	id, err := UpsertPersonalityTrait(ctx, pool, "interest", "test_trait_xyz", "testing value", "test context")
	if err != nil {
		t.Fatalf("UpsertPersonalityTrait insert: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM personality_traits WHERE id = $1", id)
	})

	// Get and verify
	traits, err := GetPersonalityTraitsByCategory(ctx, pool, "interest")
	if err != nil {
		t.Fatalf("GetPersonalityTraitsByCategory: %v", err)
	}
	var found *PersonalityTrait
	for i, tr := range traits {
		if tr.TraitKey == "test_trait_xyz" {
			found = &traits[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted trait not found")
	}
	if found.TraitValue != "testing value" {
		t.Errorf("TraitValue = %q, want %q", found.TraitValue, "testing value")
	}

	// Upsert update
	id2, err := UpsertPersonalityTrait(ctx, pool, "interest", "test_trait_xyz", "updated value", "new context")
	if err != nil {
		t.Fatalf("UpsertPersonalityTrait update: %v", err)
	}
	if id2 != id {
		t.Errorf("upsert should return same id: got %d, want %d", id2, id)
	}
	traits, _ = GetPersonalityTraitsByCategory(ctx, pool, "interest")
	for _, tr := range traits {
		if tr.TraitKey == "test_trait_xyz" {
			if tr.TraitValue != "updated value" {
				t.Errorf("after upsert: TraitValue = %q, want %q", tr.TraitValue, "updated value")
			}
			break
		}
	}
}

func TestPersonalityDelete(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	id, err := UpsertPersonalityTrait(ctx, pool, "hobby", "test_delete_xyz", "running", "")
	if err != nil {
		t.Fatalf("UpsertPersonalityTrait: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM personality_traits WHERE id = $1", id)
	})

	// Delete existing
	deleted, err := DeletePersonalityTrait(ctx, pool, "hobby", "test_delete_xyz")
	if err != nil {
		t.Fatalf("DeletePersonalityTrait: %v", err)
	}
	if !deleted {
		t.Error("DeletePersonalityTrait should return true for existing trait")
	}

	// Delete missing
	deleted, err = DeletePersonalityTrait(ctx, pool, "hobby", "test_delete_xyz")
	if err != nil {
		t.Fatalf("DeletePersonalityTrait missing: %v", err)
	}
	if deleted {
		t.Error("DeletePersonalityTrait should return false for missing trait")
	}
}

func TestFormatPersonalityForPrompt(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	// Insert known traits.
	id1, _ := UpsertPersonalityTrait(ctx, pool, "interest", "test_fmt_ai", "fascinated", "")
	id2, _ := UpsertPersonalityTrait(ctx, pool, "style", "test_fmt_tone", "casual", "with friends")
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM personality_traits WHERE id IN ($1, $2)", id1, id2)
	})

	output, err := FormatPersonalityForPrompt(ctx, pool)
	if err != nil {
		t.Fatalf("FormatPersonalityForPrompt: %v", err)
	}
	if !strings.Contains(output, "## My Personality") {
		t.Error("output missing header")
	}
	if !strings.Contains(output, "test_fmt_ai") {
		t.Error("output missing interest trait")
	}
	if !strings.Contains(output, "test_fmt_tone") {
		t.Error("output missing style trait")
	}
	if !strings.Contains(output, "(with friends)") {
		t.Error("output missing context annotation")
	}
}

func TestApplyPersonalityUpdates(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	// Seed a trait to remove.
	id, _ := UpsertPersonalityTrait(ctx, pool, "goal", "test_apply_old", "obsolete", "")
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM personality_traits WHERE category = 'goal' AND trait_key LIKE 'test_apply_%'")
		pool.Exec(context.Background(), "DELETE FROM personality_traits WHERE id = $1", id)
	})

	updates := []PersonalityUpdate{
		{Action: "set", Category: "goal", Key: "test_apply_new", Value: "learn rust"},
		{Action: "remove", Category: "goal", Key: "test_apply_old"},
		{Action: "set", Category: "", Key: "invalid", Value: "skip"}, // should be skipped
	}

	applied := ApplyPersonalityUpdates(ctx, pool, updates, "test")
	if applied != 2 {
		t.Errorf("ApplyPersonalityUpdates applied = %d, want 2", applied)
	}

	// Verify set worked.
	traits, _ := GetPersonalityTraitsByCategory(ctx, pool, "goal")
	foundNew := false
	foundOld := false
	for _, tr := range traits {
		if tr.TraitKey == "test_apply_new" {
			foundNew = true
		}
		if tr.TraitKey == "test_apply_old" {
			foundOld = true
		}
	}
	if !foundNew {
		t.Error("set action should have created test_apply_new")
	}
	if foundOld {
		t.Error("remove action should have deleted test_apply_old")
	}
}
