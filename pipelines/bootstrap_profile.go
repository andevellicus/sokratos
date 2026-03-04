package pipelines

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
)

// bootstrapOutput is the expected dual structure from the bootstrap prompt.
type bootstrapOutput struct {
	Personality []memory.PersonalityUpdate `json:"personality"`
	UserProfile json.RawMessage            `json:"user_profile"`
}

// BootstrapConfig holds dependencies for the /bootstrap command.
type BootstrapConfig struct {
	Pool          *pgxpool.Pool
	DTC           *clients.DeepThinkerClient
	EmbedEndpoint string
	EmbedModel    string
	AgentName     string
	SendFunc      func(string)               // Telegram notification on completion/failure
	OnProfile     func()                      // refresh engine profile/personality
	BgGrammarFn   memory.GrammarSubagentFunc  // entity enrichment for saved memories
	QueueFn       memory.WorkQueueFunc       // work queue for enrichment retries (nil = fire-and-forget)
}

// bootstrapResult classifies the outcome of a single bootstrap attempt.
type bootstrapResult int

const (
	bsSuccess        bootstrapResult = iota // result string is non-empty
	bsFatal                                 // non-retryable error (already logged)
	bsRetryJSON                             // JSON parse failure — retry immediately
	bsRetryTransient                        // transient DTC error — retry after backoff
)

// RunBootstrap generates an identity profile via the deep thinker. Intended
// to be called as a goroutine from the /bootstrap command handler. Notifies
// the user via SendFunc on completion or failure.
func RunBootstrap(cfg BootstrapConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutBootstrapProfile)
	defer cancel()

	// Check for existing profile.
	existing, err := memory.GetIdentityProfile(ctx, cfg.Pool)
	if err != nil {
		notify(cfg.SendFunc, fmt.Sprintf("Bootstrap failed: %v", err))
		return
	}
	if existing != "{}" {
		logger.Log.Warn("[bootstrap] overwriting existing identity profile")
	}

	// Load prompt: file override or embedded default.
	prompt, err := loadBootstrapPrompt(cfg.AgentName)
	if err != nil {
		notify(cfg.SendFunc, fmt.Sprintf("Bootstrap failed: %v", err))
		return
	}
	userContent := loadBootstrapContext()

	// Retry loop: up to 4 attempts with exponential backoff for transient errors.
	const maxAttempts = 4
	backoff := 5 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, state := bootstrapAttempt(ctx, cfg.Pool, cfg.DTC, cfg.EmbedEndpoint, cfg.EmbedModel, prompt, userContent, cfg.OnProfile, cfg.BgGrammarFn, cfg.QueueFn, attempt)

		switch state {
		case bsSuccess:
			notify(cfg.SendFunc, result)
			return

		case bsFatal:
			// Non-retryable error — notify with the result message.
			if result != "" {
				notify(cfg.SendFunc, result)
			} else {
				notify(cfg.SendFunc, "Bootstrap failed — check the logs for details.")
			}
			return

		case bsRetryJSON:
			if attempt < maxAttempts {
				logger.Log.Warnf("[bootstrap] attempt %d: JSON parse failure, retrying...", attempt)
			}

		case bsRetryTransient:
			if attempt < maxAttempts {
				logger.Log.Warnf("[bootstrap] attempt %d: transient error, retrying in %s...", attempt, backoff)
				select {
				case <-ctx.Done():
					notify(cfg.SendFunc, "Bootstrap timed out while waiting for the reasoning server.")
					return
				case <-time.After(backoff):
					backoff *= 2
				}
			}
		}
	}

	logger.Log.Errorf("[bootstrap] all %d attempts failed", maxAttempts)
	notify(cfg.SendFunc, "Bootstrap failed after multiple attempts. Please check that the reasoning server is running and try again.")
}

// notify calls sendFunc if non-nil.
func notify(sendFunc func(string), msg string) {
	if sendFunc != nil {
		sendFunc(msg)
	}
}

// loadBootstrapPrompt loads the bootstrap system prompt from a file override
// or the embedded default, replacing %AGENT_NAME% with the agent name.
func loadBootstrapPrompt(agentName string) (string, error) {
	var prompt string
	if p := os.Getenv("BOOTSTRAP_PROMPT_PATH"); p != "" {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("failed to read bootstrap prompt from %s: %w", p, err)
		}
		prompt = string(data)
	} else {
		prompt = prompts.Bootstrap
	}
	return strings.ReplaceAll(prompt, "%AGENT_NAME%", agentName), nil
}

// loadBootstrapContext loads bootstrap user content from file, env var, or default.
func loadBootstrapContext() string {
	if p := os.Getenv("BOOTSTRAP_CONTEXT_PATH"); p != "" {
		data, err := os.ReadFile(p)
		if err != nil {
			logger.Log.Warnf("[bootstrap] failed to read context file %s: %v", p, err)
			return "Generate your initial identity profile."
		}
		return strings.TrimSpace(string(data))
	}
	if c := os.Getenv("BOOTSTRAP_CONTEXT"); c != "" {
		return c
	}
	return "Generate your initial identity profile."
}

// isTransientDTCError returns true for network-level errors that may resolve
// on retry (server restarting, connection dropped, etc.).
func isTransientDTCError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "EOF") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "i/o timeout")
}

// bootstrapAttempt runs one DTC call + parse cycle. Returns (result, state).
func bootstrapAttempt(ctx context.Context, pool *pgxpool.Pool, dtc *clients.DeepThinkerClient, embedEndpoint, embedModel, prompt, userContent string, onProfile func(), bgGrammarFn memory.GrammarSubagentFunc, queueFn memory.WorkQueueFunc, attempt int) (string, bootstrapResult) {
	raw, err := dtc.CompleteNoThink(ctx, prompt, userContent, 8192)
	if err != nil {
		logger.Log.Errorf("[bootstrap] deep thinker call failed: %v", err)
		if isTransientDTCError(err) {
			return "", bsRetryTransient
		}
		// Non-transient (e.g. context cancelled, malformed request).
		return fmt.Sprintf("Bootstrap failed — reasoning server error: %v", err), bsFatal
	}

	logger.Log.Debugf("[bootstrap] raw DTC output (attempt %d, %d chars): %.500s", attempt, len(raw), raw)

	// Clean up and validate JSON.
	cleaned := textutil.CleanLLMJSON(raw)
	logger.Log.Debugf("[bootstrap] cleaned JSON (attempt %d, %d chars): %.500s", attempt, len(cleaned), cleaned)

	// Try dual structure first; fall back to legacy single-object format.
	var dual bootstrapOutput
	if err := json.Unmarshal([]byte(cleaned), &dual); err == nil && len(dual.Personality) > 0 {
		result, bErr := bootstrapDual(ctx, pool, embedEndpoint, embedModel, bgGrammarFn, queueFn, dual)
		if bErr != nil {
			logger.Log.Errorf("[bootstrap] failed: %v", bErr)
			return "Bootstrap failed — could not save profile. Check the logs for details.", bsFatal
		}
		logger.Log.Infof("[bootstrap] background bootstrap complete")
		if onProfile != nil {
			onProfile()
		}
		return result, bsSuccess
	}

	// Fallback: treat entire output as a legacy profile.
	var parsed json.RawMessage
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		logger.Log.Errorf("[bootstrap] attempt %d: not valid JSON: %v", attempt, err)
		logger.Log.Errorf("[bootstrap] attempt %d: cleaned output was: %.1000s", attempt, cleaned)
		return "", bsRetryJSON
	}

	pretty, _ := json.MarshalIndent(parsed, "", "  ")
	profileJSON := string(pretty)

	if err := memory.WriteIdentityProfile(ctx, pool, embedEndpoint, embedModel, profileJSON); err != nil {
		logger.Log.Errorf("[bootstrap] write identity profile failed: %v", err)
		return "Bootstrap failed — could not save profile. Check the logs for details.", bsFatal
	}

	logger.Log.Infof("[bootstrap] identity profile generated and written (%d bytes, legacy format)", len(profileJSON))
	if onProfile != nil {
		onProfile()
	}
	return "Identity profile bootstrapped successfully.", bsSuccess
}

// bootstrapDual processes the dual-structure bootstrap output: writes personality
// traits, extracts user preferences and recurring topics to their native stores,
// and writes a compact identity card (name + important_people only).
func bootstrapDual(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel string, bgGrammarFn memory.GrammarSubagentFunc, queueFn memory.WorkQueueFunc, dual bootstrapOutput) (string, error) {
	// Write personality traits.
	traitCount := memory.ApplyPersonalityUpdates(ctx, pool, dual.Personality, "bootstrap")

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
		}, bgGrammarFn, queueFn)
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

	// Seed default routines (structured format matching .config/routines.toml schema).
	defaultRoutines := []struct {
		Name          string
		Interval      string
		Action        *string
		Goal          *string
		SilentIfEmpty bool
	}{
		{"check-calendar", "6 hours", strPtr("search_calendar"), strPtr("Alert about any events today or tomorrow."), true},
	}
	for _, r := range defaultRoutines {
		_, err := pool.Exec(ctx,
			`INSERT INTO routines (name, interval_duration, action, goal, silent_if_empty)
			 VALUES ($1, $2::interval, $3, $4, $5)
			 ON CONFLICT (name) DO NOTHING`,
			r.Name, r.Interval, r.Action, r.Goal, r.SilentIfEmpty)
		if err != nil {
			logger.Log.Warnf("[bootstrap] failed to seed routine %s: %v", r.Name, err)
		} else {
			logger.Log.Infof("[bootstrap] seeded routine: %s (every %s)", r.Name, r.Interval)
		}
	}

	logger.Log.Infof("[bootstrap] bootstrapped %d personality traits + user profile (%d bytes)", traitCount, len(profileJSON))
	return fmt.Sprintf("Bootstrap complete: %d personality traits written, user profile initialized.", traitCount), nil
}

func strPtr(s string) *string { return &s }
