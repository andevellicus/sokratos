package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/memory"
)

// MigrateProfileToPersonality extracts personality-like fields from the existing
// identity profile and writes them to the personality_traits table. Called at
// startup when personality_traits is empty AND an identity profile exists.
// After migration, rewrites the identity profile without the extracted fields.
func MigrateProfileToPersonality(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel string) error {
	// Check if personality_traits already has data.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM personality_traits`).Scan(&count); err != nil {
		return fmt.Errorf("check personality_traits count: %w", err)
	}
	if count > 0 {
		logger.Log.Info("[migrate] personality_traits already populated, skipping migration")
		return nil
	}

	// Read the current identity profile.
	profileJSON, err := memory.GetIdentityProfile(ctx, pool)
	if err != nil {
		return fmt.Errorf("read identity profile: %w", err)
	}
	if profileJSON == "{}" {
		logger.Log.Info("[migrate] no identity profile found, skipping migration")
		return nil
	}

	// Parse the profile into a generic map.
	var profile map[string]json.RawMessage
	if err := json.Unmarshal([]byte(profileJSON), &profile); err != nil {
		return fmt.Errorf("parse identity profile: %w", err)
	}

	migrated := 0

	// Extract interests.
	if raw, ok := profile["interests"]; ok {
		var interests []string
		if err := json.Unmarshal(raw, &interests); err == nil {
			for _, interest := range interests {
				key := slugify(interest)
				if key == "" {
					continue
				}
				if _, err := memory.UpsertPersonalityTrait(ctx, pool, "interest", key, interest, ""); err != nil {
					logger.Log.Warnf("[migrate] failed to upsert interest %q: %v", key, err)
					continue
				}
				migrated++
			}
			delete(profile, "interests")
		}
	}

	// Extract hobbies.
	if raw, ok := profile["hobbies"]; ok {
		var hobbies []struct {
			Name    string `json:"name"`
			Details string `json:"details"`
		}
		if err := json.Unmarshal(raw, &hobbies); err == nil {
			for _, h := range hobbies {
				key := slugify(h.Name)
				if key == "" {
					continue
				}
				value := h.Details
				if value == "" {
					value = h.Name
				}
				if _, err := memory.UpsertPersonalityTrait(ctx, pool, "hobby", key, value, ""); err != nil {
					logger.Log.Warnf("[migrate] failed to upsert hobby %q: %v", key, err)
					continue
				}
				migrated++
			}
			delete(profile, "hobbies")
		}
	}

	// Extract goals.
	if raw, ok := profile["goals"]; ok {
		var goals []string
		if err := json.Unmarshal(raw, &goals); err == nil {
			for _, goal := range goals {
				key := slugify(goal)
				if key == "" {
					continue
				}
				if _, err := memory.UpsertPersonalityTrait(ctx, pool, "goal", key, goal, ""); err != nil {
					logger.Log.Warnf("[migrate] failed to upsert goal %q: %v", key, err)
					continue
				}
				migrated++
			}
			delete(profile, "goals")
		}
	}

	// Extract preferences that are personality-like (communication style, etc.).
	if raw, ok := profile["preferences"]; ok {
		var prefs []struct {
			Key     string `json:"key"`
			Value   string `json:"value"`
			Context string `json:"context"`
		}
		if err := json.Unmarshal(raw, &prefs); err == nil {
			var remaining []struct {
				Key     string `json:"key"`
				Value   string `json:"value"`
				Context string `json:"context"`
			}
			for _, p := range prefs {
				if isStylePreference(p.Key) {
					key := slugify(p.Key)
					if key == "" {
						continue
					}
					if _, err := memory.UpsertPersonalityTrait(ctx, pool, "style", key, p.Value, p.Context); err != nil {
						logger.Log.Warnf("[migrate] failed to upsert style pref %q: %v", key, err)
						remaining = append(remaining, p)
						continue
					}
					migrated++
				} else {
					remaining = append(remaining, p)
				}
			}
			if len(remaining) > 0 {
				if data, err := json.Marshal(remaining); err == nil {
					profile["preferences"] = data
				}
			} else {
				delete(profile, "preferences")
			}
		}
	}

	if migrated == 0 {
		logger.Log.Info("[migrate] no personality-like fields found in profile, skipping")
		return nil
	}

	// Rewrite the slimmed profile.
	slimmed, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal slimmed profile: %w", err)
	}

	if err := memory.WriteIdentityProfile(ctx, pool, embedEndpoint, embedModel, string(slimmed)); err != nil {
		return fmt.Errorf("write slimmed profile: %w", err)
	}

	logger.Log.Infof("[migrate] migrated %d personality traits from profile, profile slimmed", migrated)
	return nil
}

// styleKeywords identifies preference keys that relate to communication style
// or agent personality rather than user-specific preferences.
var styleKeywords = []string{
	"humor", "style", "tone", "verbos", "communicat", "sarcas", "formal",
	"push_back", "pushback", "directness", "brevity", "warmth",
}

func isStylePreference(key string) bool {
	lower := strings.ToLower(key)
	for _, kw := range styleKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// slugifyRe matches non-alphanumeric, non-hyphen, non-underscore characters.
var slugifyRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// slugify converts a string to a lowercase slug suitable for use as a trait key.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugifyRe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}
