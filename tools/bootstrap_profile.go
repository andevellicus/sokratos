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
// profile via the deep thinker and writes it to the database. The heavy DTC
// call runs in a background goroutine so the orchestrator can respond
// immediately; sendFunc delivers a Telegram notification on completion.
func NewBootstrapProfile(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel, agentName string, sendFunc func(string), onProfile func()) ToolFunc {
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

		// Run the heavy DTC call in the background so the orchestrator
		// can respond immediately ("on it") instead of blocking 2-3 min.
		go bootstrapBackground(pool, dtc, embedEndpoint, embedModel, prompt, userContent, sendFunc, onProfile)

		return "Profile generation started in the background. I'll notify you when it's ready.", nil
	}
}

// bootstrapBackground runs the deep thinker call and writes results. Called
// as a goroutine from NewBootstrapProfile. Retries once on JSON parse failure.
func bootstrapBackground(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel, prompt, userContent string, sendFunc func(string), onProfile func()) {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutBootstrapProfile)
	defer cancel()

	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, done := bootstrapAttempt(ctx, pool, dtc, embedEndpoint, embedModel, prompt, userContent, sendFunc, onProfile, attempt)
		if done {
			if result != "" && sendFunc != nil {
				sendFunc(result)
			}
			return
		}
		// JSON parse failure — retry if we have attempts left.
		if attempt < maxAttempts {
			logger.Log.Warnf("[bootstrap] attempt %d failed, retrying...", attempt)
		}
	}

	logger.Log.Errorf("[bootstrap] all %d attempts failed", maxAttempts)
	if sendFunc != nil {
		sendFunc("Bootstrap failed after retries — the model couldn't produce valid JSON. Please try again later.")
	}
}

// bootstrapAttempt runs one DTC call + parse cycle. Returns (result, done).
// done=true means either success (result is non-empty) or a non-retryable error
// (result is empty, error already sent via sendFunc). done=false means a JSON
// parse failure that should be retried.
func bootstrapAttempt(ctx context.Context, pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel, prompt, userContent string, sendFunc func(string), onProfile func(), attempt int) (string, bool) {
	raw, err := dtc.Complete(ctx, prompt, userContent, 8192)
	if err != nil {
		logger.Log.Errorf("[bootstrap] deep thinker call failed: %v", err)
		if sendFunc != nil {
			sendFunc("Bootstrap failed — the reasoning server timed out. Please try again later.")
		}
		return "", true // non-retryable
	}

	logger.Log.Debugf("[bootstrap] raw DTC output (attempt %d, %d chars): %.500s", attempt, len(raw), raw)

	// Clean up and validate JSON.
	cleaned := textutil.CleanLLMJSON(raw)
	logger.Log.Debugf("[bootstrap] cleaned JSON (attempt %d, %d chars): %.500s", attempt, len(cleaned), cleaned)

	// Try dual structure first; fall back to legacy single-object format.
	var dual bootstrapOutput
	if err := json.Unmarshal([]byte(cleaned), &dual); err == nil && len(dual.Personality) > 0 {
		result, bErr := bootstrapDual(ctx, pool, embedEndpoint, embedModel, dual)
		if bErr != nil {
			logger.Log.Errorf("[bootstrap] failed: %v", bErr)
			if sendFunc != nil {
				sendFunc("Bootstrap failed — could not save profile. Check the logs for details.")
			}
			return "", true // non-retryable
		}
		logger.Log.Infof("[bootstrap] background bootstrap complete")
		if onProfile != nil {
			onProfile()
		}
		return result, true
	}

	// Fallback: treat entire output as a legacy profile.
	var parsed json.RawMessage
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		logger.Log.Errorf("[bootstrap] attempt %d: not valid JSON: %v", attempt, err)
		logger.Log.Errorf("[bootstrap] attempt %d: cleaned output was: %.1000s", attempt, cleaned)
		return "", false // retryable
	}

	pretty, _ := json.MarshalIndent(parsed, "", "  ")
	profileJSON := string(pretty)

	if err := memory.WriteIdentityProfile(ctx, pool, embedEndpoint, embedModel, profileJSON); err != nil {
		logger.Log.Errorf("[bootstrap] write identity profile failed: %v", err)
		if sendFunc != nil {
			sendFunc("Bootstrap failed — could not save profile. Check the logs for details.")
		}
		return "", true // non-retryable
	}

	logger.Log.Infof("[bootstrap] identity profile generated and written (%d bytes, legacy format)", len(profileJSON))
	if onProfile != nil {
		onProfile()
	}
	return "Identity profile bootstrapped successfully.", true
}

// bootstrapDual processes the dual-structure bootstrap output: writes personality
// traits, extracts user preferences and recurring topics to their native stores,
// and writes a compact identity card (name + important_people only).
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

	// Parse the DT output to extract fields and build compact card.
	var up struct {
		Name            string `json:"name"`
		ImportantPeople []struct {
			Name     string `json:"name"`
			Relation string `json:"relation"`
		} `json:"important_people"`
		RecurringTopics []string `json:"recurring_topics"`
		UserPreferences []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"user_preferences"`
		LastConsolidated string `json:"last_consolidated"`
	}
	if len(dual.UserProfile) > 0 {
		if err := json.Unmarshal(dual.UserProfile, &up); err != nil {
			logger.Log.Warnf("[bootstrap] failed to parse user_profile: %v", err)
		}
	}

	// Save user_preferences into the native postgres table (not in card).
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

	// Save recurring_topics as individual tagged memories.
	topicCount := 0
	for _, topic := range up.RecurringTopics {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			continue
		}
		memory.SaveToMemoryWithSalienceAsync(pool, memory.MemoryWriteRequest{
			Summary:       fmt.Sprintf("User's recurring interest/topic: %s", topic),
			Tags:          []string{"interest", "user_knowledge", "recurring_topic"},
			Salience:      7,
			MemoryType:    "general",
			Source:        "bootstrap",
			EmbedEndpoint: embedEndpoint,
			EmbedModel:    embedModel,
		}, nil)
		topicCount++
	}
	if topicCount > 0 {
		logger.Log.Infof("[bootstrap] saved %d recurring topics as memories", topicCount)
	}

	// Build compact identity card (only name + important_people + timestamp).
	card := map[string]any{}
	if up.Name != "" {
		card["name"] = up.Name
	}
	if len(up.ImportantPeople) > 0 {
		card["important_people"] = up.ImportantPeople
	}
	if up.LastConsolidated != "" {
		card["last_consolidated"] = up.LastConsolidated
	}

	profileJSON := "{}"
	if len(card) > 0 {
		pretty, _ := json.MarshalIndent(card, "", "  ")
		profileJSON = string(pretty)
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
