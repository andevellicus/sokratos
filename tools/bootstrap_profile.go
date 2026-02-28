package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
)

// bootstrapOutput is the expected dual structure from the bootstrap prompt.
type bootstrapOutput struct {
	Personality []bootstrapTrait `json:"personality"`
	UserProfile json.RawMessage  `json:"user_profile"`
}

type bootstrapTrait struct {
	Category string `json:"category"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	Context  string `json:"context,omitempty"`
}

// NewBootstrapProfile returns a ToolFunc that generates a rich initial identity
// profile via the deep thinker and writes it to the database. The prompt and
// user context can be overridden via env vars (BOOTSTRAP_PROMPT_PATH,
// BOOTSTRAP_CONTEXT_PATH, BOOTSTRAP_CONTEXT). Should be permission-gated.
func NewBootstrapProfile(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel, agentName string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		// Check for existing profile.
		existing, err := memory.GetIdentityProfile(ctx, pool)
		if err != nil {
			return fmt.Sprintf("Failed to read existing profile: %v", err), nil
		}
		if existing != "{}" {
			logger.Log.Warn("[bootstrap] overwriting existing identity profile")
		}

		// Load prompt: file override or embedded default.
		var prompt string
		if p := os.Getenv("BOOTSTRAP_PROMPT_PATH"); p != "" {
			data, err := os.ReadFile(p)
			if err != nil {
				return fmt.Sprintf("Failed to read bootstrap prompt from %s: %v", p, err), nil
			}
			prompt = string(data)
		} else {
			prompt = prompts.Bootstrap
		}
		prompt = strings.ReplaceAll(prompt, "%AGENT_NAME%", agentName)

		// User content: file path > env var > default.
		var userContent string
		if p := os.Getenv("BOOTSTRAP_CONTEXT_PATH"); p != "" {
			data, err := os.ReadFile(p)
			if err != nil {
				return fmt.Sprintf("Failed to read bootstrap context from %s: %v", p, err), nil
			}
			userContent = strings.TrimSpace(string(data))
		} else if c := os.Getenv("BOOTSTRAP_CONTEXT"); c != "" {
			userContent = c
		} else {
			userContent = "Generate your initial identity profile."
		}

		// Call deep thinker with a generous timeout.
		dtcCtx, cancel := context.WithTimeout(ctx, TimeoutBootstrapProfile)
		defer cancel()

		raw, err := dtc.CompleteNoThink(dtcCtx, prompt, userContent, 4096)
		if err != nil {
			return fmt.Sprintf("Deep thinker call failed: %v", err), nil
		}

		// Clean up and validate JSON.
		raw = textutil.CleanLLMJSON(raw)

		// Try dual structure first; fall back to legacy single-object format.
		var dual bootstrapOutput
		if err := json.Unmarshal([]byte(raw), &dual); err == nil && len(dual.Personality) > 0 {
			return bootstrapDual(ctx, pool, embedEndpoint, embedModel, dual)
		}

		// Fallback: treat entire output as a legacy profile.
		var parsed json.RawMessage
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return fmt.Sprintf("Generated output is not valid JSON: %v\nRaw: %s", err, raw), nil
		}

		pretty, _ := json.MarshalIndent(parsed, "", "  ")
		profileJSON := string(pretty)

		if err := memory.WriteIdentityProfile(ctx, pool, embedEndpoint, embedModel, profileJSON); err != nil {
			return "", fmt.Errorf("write identity profile: %w", err)
		}

		logger.Log.Infof("[bootstrap] identity profile generated and written (%d bytes, legacy format)", len(profileJSON))
		return "Identity profile bootstrapped successfully (legacy format):\n" + profileJSON, nil
	}
}

// bootstrapDual processes the dual-structure bootstrap output: writes personality
// traits and user profile separately.
func bootstrapDual(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel string, dual bootstrapOutput) (string, error) {
	// Write personality traits.
	traitCount := 0
	for _, t := range dual.Personality {
		if t.Category == "" || t.Key == "" || t.Value == "" {
			continue
		}
		if _, err := memory.UpsertPersonalityTrait(ctx, pool, t.Category, t.Key, t.Value, t.Context); err != nil {
			logger.Log.Warnf("[bootstrap] failed to upsert trait %s/%s: %v", t.Category, t.Key, err)
			continue
		}
		traitCount++
	}

	// Write user profile (may be mostly empty at bootstrap).
	var profileJSON string
	if len(dual.UserProfile) > 0 {
		// First pass: extract structured preferences directly to the Postgres table
		var up struct {
			Name            string   `json:"name"`
			ImportantPeople []string `json:"important_people"`
			RecurringTopics []string `json:"recurring_topics"`
			UserPreferences []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"user_preferences"`
		}

		// If unmarshalling succeeds, build a markdown representation for the memory identity,
		// and save preferences. If it fails, fallback to the raw JSON string.
		if err := json.Unmarshal(dual.UserProfile, &up); err == nil {
			// Save preferences into postgres natively
			if len(up.UserPreferences) > 0 {
				for _, pref := range up.UserPreferences {
					if pref.Key == "" || pref.Value == "" {
						continue
					}
					_, err := pool.Exec(ctx,
						`INSERT INTO user_preferences (key, value, updated_at)
						 VALUES ($1, $2, NOW())
						 ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()`,
						pref.Key, pref.Value)
					if err != nil {
						logger.Log.Warnf("[bootstrap] failed to persist user preference %s: %v", pref.Key, err)
					}
				}
				logger.Log.Infof("[bootstrap] persisted %d user preferences", len(up.UserPreferences))
			}

			// Generate a clean markdown profile identity
			var builder strings.Builder
			if up.Name != "" {
				builder.WriteString(fmt.Sprintf("**Name:** %s\n", up.Name))
			}
			if len(up.ImportantPeople) > 0 {
				builder.WriteString("**Important People:**\n")
				for _, person := range up.ImportantPeople {
					builder.WriteString(fmt.Sprintf("- %s\n", person))
				}
			}
			if len(up.RecurringTopics) > 0 {
				builder.WriteString("**Recurring Topics:**\n")
				for _, topic := range up.RecurringTopics {
					builder.WriteString(fmt.Sprintf("- %s\n", topic))
				}
			}

			profileJSON = strings.TrimSpace(builder.String())
			// Fallback if markdown was completely empty
			if profileJSON == "" {
				pretty, _ := json.MarshalIndent(dual.UserProfile, "", "  ")
				profileJSON = string(pretty)
			}
		} else {
			// Failed to unmarshal properly; fallback to raw JSON
			pretty, _ := json.MarshalIndent(dual.UserProfile, "", "  ")
			profileJSON = string(pretty)
		}
	} else {
		profileJSON = "{}"
	}

	if err := memory.WriteIdentityProfile(ctx, pool, embedEndpoint, embedModel, profileJSON); err != nil {
		return "", fmt.Errorf("write identity profile: %w", err)
	}

	// Seed default routines.
	defaultRoutines := []struct {
		Name        string
		Interval    string
		Instruction string
	}{
		{"check-calendar", "4 hours", "Check upcoming calendar events using check_calendar. Alert about any events today or tomorrow."},
	}
	for _, r := range defaultRoutines {
		_, err := pool.Exec(ctx,
			`INSERT INTO routines (name, interval_duration, instruction)
			 VALUES ($1, $2::interval, $3)
			 ON CONFLICT (name) DO NOTHING`,
			r.Name, r.Interval, r.Instruction)
		if err != nil {
			logger.Log.Warnf("[bootstrap] failed to seed routine %s: %v", r.Name, err)
		} else {
			logger.Log.Infof("[bootstrap] seeded routine: %s (every %s)", r.Name, r.Interval)
		}
	}

	logger.Log.Infof("[bootstrap] bootstrapped %d personality traits + user profile (%d bytes)", traitCount, len(profileJSON))
	return fmt.Sprintf("Bootstrap complete: %d personality traits written, user profile initialized.", traitCount), nil
}
