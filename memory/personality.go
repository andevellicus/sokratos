package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"sokratos/logger"
)

// PersonalityTrait represents a single personality trait stored in the database.
type PersonalityTrait struct {
	ID         int
	Category   string
	TraitKey   string
	TraitValue string
	Context    string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ValidPersonalityCategories lists the allowed category values.
var ValidPersonalityCategories = map[string]bool{
	"interest":   true,
	"hobby":      true,
	"goal":       true,
	"worldview":  true,
	"style":      true,
	"preference": true,
}

// GetAllPersonalityTraits returns all personality traits ordered by category and key.
func GetAllPersonalityTraits(ctx context.Context, db *pgxpool.Pool) ([]PersonalityTrait, error) {
	rows, err := db.Query(ctx,
		`SELECT id, category, trait_key, trait_value, COALESCE(context, ''), created_at, updated_at
		 FROM personality_traits
		 ORDER BY category, trait_key`)
	if err != nil {
		return nil, fmt.Errorf("query personality traits: %w", err)
	}
	defer rows.Close()

	var traits []PersonalityTrait
	for rows.Next() {
		var t PersonalityTrait
		if err := rows.Scan(&t.ID, &t.Category, &t.TraitKey, &t.TraitValue, &t.Context, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan personality trait: %w", err)
		}
		traits = append(traits, t)
	}
	return traits, rows.Err()
}

// GetPersonalityTraitsByCategory returns traits filtered by category.
func GetPersonalityTraitsByCategory(ctx context.Context, db *pgxpool.Pool, category string) ([]PersonalityTrait, error) {
	rows, err := db.Query(ctx,
		`SELECT id, category, trait_key, trait_value, COALESCE(context, ''), created_at, updated_at
		 FROM personality_traits
		 WHERE category = $1
		 ORDER BY trait_key`, category)
	if err != nil {
		return nil, fmt.Errorf("query personality traits by category: %w", err)
	}
	defer rows.Close()

	var traits []PersonalityTrait
	for rows.Next() {
		var t PersonalityTrait
		if err := rows.Scan(&t.ID, &t.Category, &t.TraitKey, &t.TraitValue, &t.Context, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan personality trait: %w", err)
		}
		traits = append(traits, t)
	}
	return traits, rows.Err()
}

// UpsertPersonalityTrait inserts or updates a personality trait. Returns the trait ID.
func UpsertPersonalityTrait(ctx context.Context, db *pgxpool.Pool, category, key, value, traitContext string) (int, error) {
	var id int
	err := db.QueryRow(ctx,
		`INSERT INTO personality_traits (category, trait_key, trait_value, context)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (category, trait_key)
		 DO UPDATE SET trait_value = EXCLUDED.trait_value,
		               context = EXCLUDED.context,
		               updated_at = now()
		 RETURNING id`,
		category, key, value, traitContext,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert personality trait: %w", err)
	}
	logger.Log.Infof("[personality] upserted %s/%s (id=%d)", category, key, id)
	return id, nil
}

// DeletePersonalityTrait removes a trait by category and key. Returns true if a row was deleted.
func DeletePersonalityTrait(ctx context.Context, db *pgxpool.Pool, category, key string) (bool, error) {
	tag, err := db.Exec(ctx,
		`DELETE FROM personality_traits WHERE category = $1 AND trait_key = $2`,
		category, key,
	)
	if err != nil {
		return false, fmt.Errorf("delete personality trait: %w", err)
	}
	deleted := tag.RowsAffected() > 0
	if deleted {
		logger.Log.Infof("[personality] deleted %s/%s", category, key)
	}
	return deleted, nil
}

// PersonalityUpdate describes a single personality mutation (set or remove).
// Used by both consolidation and bootstrap to apply personality changes.
type PersonalityUpdate struct {
	Action   string `json:"action"`
	Category string `json:"category"`
	Key      string `json:"key"`
	Value    string `json:"value,omitempty"`
	Context  string `json:"context,omitempty"`
}

// ApplyPersonalityUpdates processes a slice of personality mutations (set/remove).
// Returns the count of successfully applied updates.
func ApplyPersonalityUpdates(ctx context.Context, db *pgxpool.Pool, updates []PersonalityUpdate, logPrefix string) int {
	applied := 0
	for _, u := range updates {
		action := strings.ToLower(u.Action)
		if action == "" {
			action = "set" // default for bootstrap-style inputs without explicit action
		}
		switch action {
		case "set":
			if u.Category == "" || u.Key == "" || u.Value == "" {
				continue
			}
			if _, err := UpsertPersonalityTrait(ctx, db, u.Category, u.Key, u.Value, u.Context); err != nil {
				logger.Log.Warnf("[%s] failed to upsert trait %s/%s: %v", logPrefix, u.Category, u.Key, err)
				continue
			}
			applied++
		case "remove":
			if u.Category == "" || u.Key == "" {
				continue
			}
			if _, err := DeletePersonalityTrait(ctx, db, u.Category, u.Key); err != nil {
				logger.Log.Warnf("[%s] failed to delete trait %s/%s: %v", logPrefix, u.Category, u.Key, err)
				continue
			}
			applied++
		}
	}
	if applied > 0 {
		logger.Log.Infof("[%s] applied %d personality updates", logPrefix, applied)
	}
	return applied
}

// categoryDisplayOrder defines the preferred display order for categories.
var categoryDisplayOrder = []string{"worldview", "interest", "hobby", "goal", "style", "preference"}

// FormatPersonalityForPrompt returns a markdown-formatted personality section
// suitable for injection into the system prompt. Returns "" when no traits exist.
func FormatPersonalityForPrompt(ctx context.Context, db *pgxpool.Pool) (string, error) {
	traits, err := GetAllPersonalityTraits(ctx, db)
	if err != nil {
		return "", err
	}
	if len(traits) == 0 {
		return "", nil
	}

	// Group by category.
	grouped := make(map[string][]PersonalityTrait)
	for _, t := range traits {
		grouped[t.Category] = append(grouped[t.Category], t)
	}

	// Build ordered category list: known categories first in display order, then any extras alphabetically.
	var categories []string
	seen := make(map[string]bool)
	for _, cat := range categoryDisplayOrder {
		if _, ok := grouped[cat]; ok {
			categories = append(categories, cat)
			seen[cat] = true
		}
	}
	var extras []string
	for cat := range grouped {
		if !seen[cat] {
			extras = append(extras, cat)
		}
	}
	sort.Strings(extras)
	categories = append(categories, extras...)

	var b strings.Builder
	b.WriteString("## My Personality\n")

	for _, cat := range categories {
		traits := grouped[cat]
		fmt.Fprintf(&b, "\n### %s\n", cases.Title(language.English).String(cat))
		for _, t := range traits {
			if t.Context != "" {
				fmt.Fprintf(&b, "- %s: %s (%s)\n", t.TraitKey, t.TraitValue, t.Context)
			} else {
				fmt.Fprintf(&b, "- %s: %s\n", t.TraitKey, t.TraitValue)
			}
		}
	}

	return b.String(), nil
}
