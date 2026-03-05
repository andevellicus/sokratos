package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/memory"
)

type managePersonalityArgs struct {
	Action   string `json:"action"`
	Category string `json:"category"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	Context  string `json:"context"`
}

// NewManagePersonality returns a ToolFunc for viewing and evolving personality traits.
// The refreshFn callback updates the cached personality in the engine after mutations.
func NewManagePersonality(pool *pgxpool.Pool, refreshFn func()) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		a, err := ParseArgs[managePersonalityArgs](args)
		if err != nil {
			return err.Error(), nil
		}

		switch strings.ToLower(a.Action) {
		case "set":
			return personalitySet(ctx, pool, a, refreshFn)
		case "remove":
			return personalityRemove(ctx, pool, a, refreshFn)
		case "list":
			return personalityList(ctx, pool, a)
		default:
			return fmt.Sprintf("Unknown action %q. Use: set, remove, list.", a.Action), nil
		}
	}
}

func personalitySet(ctx context.Context, pool *pgxpool.Pool, a managePersonalityArgs, refreshFn func()) (string, error) {
	if a.Category == "" {
		return "Category is required for set. Valid: interest, hobby, goal, worldview, style, preference.", nil
	}
	if !memory.ValidPersonalityCategories[a.Category] {
		return fmt.Sprintf("Invalid category %q. Valid: interest, hobby, goal, worldview, style, preference.", a.Category), nil
	}
	if a.Key == "" {
		return "Key is required for set.", nil
	}
	if a.Value == "" {
		return "Value is required for set.", nil
	}

	id, err := memory.UpsertPersonalityTrait(ctx, pool, a.Category, a.Key, a.Value, a.Context)
	if err != nil {
		return fmt.Sprintf("Failed to set trait: %v", err), nil
	}

	if refreshFn != nil {
		refreshFn()
	}

	return fmt.Sprintf("Personality trait set: %s/%s (id=%d)", a.Category, a.Key, id), nil
}

func personalityRemove(ctx context.Context, pool *pgxpool.Pool, a managePersonalityArgs, refreshFn func()) (string, error) {
	if a.Category == "" {
		return "Category is required for remove.", nil
	}
	if a.Key == "" {
		return "Key is required for remove.", nil
	}

	deleted, err := memory.DeletePersonalityTrait(ctx, pool, a.Category, a.Key)
	if err != nil {
		return fmt.Sprintf("Failed to remove trait: %v", err), nil
	}
	if !deleted {
		return fmt.Sprintf("No trait found for %s/%s.", a.Category, a.Key), nil
	}

	if refreshFn != nil {
		refreshFn()
	}

	return fmt.Sprintf("Personality trait removed: %s/%s", a.Category, a.Key), nil
}

func personalityList(ctx context.Context, pool *pgxpool.Pool, a managePersonalityArgs) (string, error) {
	var traits []memory.PersonalityTrait
	var err error

	if a.Category != "" {
		traits, err = memory.GetPersonalityTraitsByCategory(ctx, pool, a.Category)
	} else {
		traits, err = memory.GetAllPersonalityTraits(ctx, pool)
	}
	if err != nil {
		return fmt.Sprintf("Failed to list traits: %v", err), nil
	}

	if len(traits) == 0 {
		if a.Category != "" {
			return fmt.Sprintf("No personality traits found in category %q.", a.Category), nil
		}
		return "No personality traits found.", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Personality traits (%d):\n", len(traits))
	for _, t := range traits {
		if t.Context != "" {
			fmt.Fprintf(&b, "- [%s] %s: %s (%s)\n", t.Category, t.TraitKey, t.TraitValue, t.Context)
		} else {
			fmt.Fprintf(&b, "- [%s] %s: %s\n", t.Category, t.TraitKey, t.TraitValue)
		}
	}
	return b.String(), nil
}
