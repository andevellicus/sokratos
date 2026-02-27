package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
)

const salienceThreshold = 8

// validateProfile checks that a profile map has minimum required structure.
func validateProfile(profile map[string]any) error {
	if _, ok := profile["name"]; !ok {
		return fmt.Errorf("profile missing required 'name' key")
	}
	if len(profile) < 2 {
		return fmt.Errorf("profile has only %d top-level key(s), need at least 2", len(profile))
	}
	return nil
}

// mergeProfileJSON merges updates into the current profile JSON.
// Scalars are overwritten. Arrays of objects are upserted by identifier field.
// Arrays of strings are unioned (case-insensitive dedup).
func mergeProfileJSON(current, updates string) (string, error) {
	var currentMap, updatesMap map[string]any

	if err := json.Unmarshal([]byte(current), &currentMap); err != nil {
		// If current profile is unparseable, start from empty.
		currentMap = make(map[string]any)
	}
	if err := json.Unmarshal([]byte(updates), &updatesMap); err != nil {
		return "", fmt.Errorf("parse profile updates: %w", err)
	}

	// Known array-of-objects fields and their identifier keys.
	objectArrayIDs := map[string]string{
		"important_people": "name",
		"user_preferences": "key",
	}
	// Known array-of-strings fields.
	stringArrays := map[string]bool{
		"recurring_topics": true,
	}

	for key, updVal := range updatesMap {
		if idField, ok := objectArrayIDs[key]; ok {
			currentMap[key] = mergeObjectArray(currentMap[key], updVal, idField)
		} else if stringArrays[key] {
			currentMap[key] = mergeStringArray(currentMap[key], updVal)
		} else {
			// Scalar or unknown type: overwrite.
			currentMap[key] = updVal
		}
	}

	return marshalProfileOrdered(currentMap)
}

// profileKeyOrder defines the canonical key order for identity profiles.
var profileKeyOrder = []string{
	"name",
	"important_people",
	"recurring_topics",
	"user_preferences",
	"last_consolidated",
}

// marshalProfileOrdered serializes a profile map with canonical key ordering
// instead of Go's default alphabetical map ordering.
func marshalProfileOrdered(m map[string]any) (string, error) {
	ordered := make([]string, 0, len(m))
	seen := make(map[string]bool, len(profileKeyOrder))

	// Canonical keys first, in order.
	for _, k := range profileKeyOrder {
		if v, ok := m[k]; ok {
			entry, err := json.MarshalIndent(v, "  ", "  ")
			if err != nil {
				return "", fmt.Errorf("marshal key %q: %w", k, err)
			}
			ordered = append(ordered, fmt.Sprintf("  %q: %s", k, entry))
			seen[k] = true
		}
	}

	// Any remaining keys appended at the end.
	for k, v := range m {
		if seen[k] {
			continue
		}
		entry, err := json.MarshalIndent(v, "  ", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal key %q: %w", k, err)
		}
		ordered = append(ordered, fmt.Sprintf("  %q: %s", k, entry))
	}

	return "{\n" + strings.Join(ordered, ",\n") + "\n}", nil
}

// mergeObjectArray upserts objects by a case-insensitive identifier field.
func mergeObjectArray(currentVal, updateVal any, idField string) any {
	currentArr := toObjectSlice(currentVal)
	updateArr := toObjectSlice(updateVal)

	// Index existing entries by lowercase identifier.
	index := make(map[string]int, len(currentArr))
	for i, obj := range currentArr {
		if id, ok := obj[idField]; ok {
			index[strings.ToLower(fmt.Sprint(id))] = i
		}
	}

	for _, upd := range updateArr {
		id, ok := upd[idField]
		if !ok {
			currentArr = append(currentArr, upd)
			continue
		}
		key := strings.ToLower(fmt.Sprint(id))
		if idx, exists := index[key]; exists {
			// Upsert: merge fields into existing entry.
			maps.Copy(currentArr[idx], upd)
		} else {
			index[key] = len(currentArr)
			currentArr = append(currentArr, upd)
		}
	}

	return currentArr
}

// mergeStringArray unions two string arrays with case-insensitive dedup.
func mergeStringArray(currentVal, updateVal any) any {
	currentArr := toStringSlice(currentVal)
	updateArr := toStringSlice(updateVal)

	seen := make(map[string]bool, len(currentArr))
	for _, s := range currentArr {
		seen[strings.ToLower(s)] = true
	}
	for _, s := range updateArr {
		if !seen[strings.ToLower(s)] {
			currentArr = append(currentArr, s)
			seen[strings.ToLower(s)] = true
		}
	}
	return currentArr
}

// toObjectSlice converts an any to []map[string]any.
func toObjectSlice(v any) []map[string]any {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			result = append(result, m)
		}
	}
	return result
}

// toStringSlice converts an any to []string.
func toStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

// ConsolidateOpts configures a consolidation run.
type ConsolidateOpts struct {
	SalienceThreshold int
	MemoryLimit       int
	Timeout           time.Duration
	MaxPromptChars    int // soft cap on user-content chars to avoid exceeding model context; 0 = 14000
}

// consolidationOutput is the expected dual structure from the consolidation prompt.
type consolidationOutput struct {
	ProfileUpdates     json.RawMessage     `json:"profile_updates"`
	UserProfile        json.RawMessage     `json:"user_profile"`         // legacy fallback
	PersonalityUpdates []personalityUpdate `json:"personality_updates"`
}

// profileData returns whichever of ProfileUpdates or UserProfile is present.
func (c consolidationOutput) profileData() json.RawMessage {
	if len(c.ProfileUpdates) > 0 {
		return c.ProfileUpdates
	}
	return c.UserProfile
}

type personalityUpdate struct {
	Action   string `json:"action"`
	Category string `json:"category"`
	Key      string `json:"key"`
	Value    string `json:"value,omitempty"`
	Context  string `json:"context,omitempty"`
}

// ConsolidateCore is the shared consolidation pipeline: query high-salience
// memories → read profile + personality → build prompt → call deep thinker →
// parse dual output → write profile + apply personality updates.
// When memories exceed the context budget, they are processed in batches with
// the profile incrementally updated between rounds.
// Returns the count of memories synthesized.
func ConsolidateCore(ctx context.Context, pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, opts ConsolidateOpts) (int, error) {
	memories, err := QueryHighSalienceMemories(ctx, pool, opts.SalienceThreshold, opts.MemoryLimit)
	if err != nil {
		return 0, fmt.Errorf("query high-salience memories: %w", err)
	}
	if len(memories) == 0 {
		return 0, nil
	}

	currentProfile, err := memory.GetIdentityProfile(ctx, pool)
	if err != nil {
		logger.Log.Warnf("[consolidate] failed to read profile from DB: %v", err)
		currentProfile = "{}"
	}

	// Z1 runs with 32K context. With 4096 max output tokens and ~4 chars/token,
	// the input budget is ~28K tokens ≈ 112K chars. 50K is conservative.
	promptBudget := opts.MaxPromptChars
	if promptBudget <= 0 {
		promptBudget = 50000
	}

	totalProcessed := 0
	remaining := memories
	batch := 0

	for len(remaining) > 0 {
		batch++

		// Reload personality traits each round (previous batch may have updated them).
		traits, tErr := memory.GetAllPersonalityTraits(ctx, pool)
		if tErr != nil {
			logger.Log.Warnf("[consolidate] failed to read personality traits: %v", tErr)
		}

		fixedPrompt := buildConsolidationPrompt(currentProfile, traits)
		fixedLen := fixedPrompt.Len() + len(prompts.Consolidation)
		memBudget := promptBudget - fixedLen
		if memBudget < 500 {
			memBudget = 500
		}

		// Fill this batch up to the memory budget.
		included := 0
		used := 0
		for i, m := range remaining {
			line := fmt.Sprintf("%d. %s\n", i+1, m)
			if used+len(line) > memBudget && included > 0 {
				break
			}
			fixedPrompt.WriteString(line)
			used += len(line)
			included++
		}
		remaining = remaining[included:]

		if batch > 1 || len(remaining) > 0 {
			logger.Log.Infof("[consolidate] batch %d: processing %d memories (%d remaining)", batch, included, len(remaining))
		}

		raw, cErr := dtc.CompleteNoThink(ctx, strings.TrimSpace(prompts.Consolidation), fixedPrompt.String(), 4096)
		if cErr != nil {
			return totalProcessed, fmt.Errorf("consolidation request (batch %d): %w", batch, cErr)
		}

		raw = textutil.CleanLLMJSON(raw)

		updatedProfile, n, aErr := applyConsolidationResult(ctx, pool, embedEndpoint, embedModel, raw, currentProfile, included)
		if aErr != nil {
			return totalProcessed, fmt.Errorf("apply consolidation (batch %d): %w", batch, aErr)
		}
		totalProcessed += n
		if updatedProfile != "" {
			currentProfile = updatedProfile
		}
	}

	return totalProcessed, nil
}

// buildConsolidationPrompt constructs the fixed (non-memory) portion of the
// consolidation prompt. Returns a builder ready for memory lines to be appended.
func buildConsolidationPrompt(currentProfile string, traits []memory.PersonalityTrait) strings.Builder {
	var b strings.Builder
	b.WriteString("CURRENT USER PROFILE:\n")
	b.WriteString(currentProfile)

	b.WriteString("\n\nCURRENT PERSONALITY:\n")
	if len(traits) == 0 {
		b.WriteString("(none)\n")
	} else {
		type traitJSON struct {
			Category string `json:"category"`
			Key      string `json:"key"`
			Value    string `json:"value"`
			Context  string `json:"context,omitempty"`
		}
		traitList := make([]traitJSON, len(traits))
		for i, t := range traits {
			traitList[i] = traitJSON{
				Category: t.Category,
				Key:      t.TraitKey,
				Value:    t.TraitValue,
				Context:  t.Context,
			}
		}
		traitData, err := json.MarshalIndent(traitList, "", "  ")
		if err != nil {
			b.WriteString("(none)\n")
		} else {
			b.Write(traitData)
			b.WriteByte('\n')
		}
	}

	b.WriteString("\nHIGH-SALIENCE MEMORIES:\n")
	fmt.Fprintf(&b, "(Current time: %s)\n\n", time.Now().Format(time.RFC3339))
	return b
}

// applyConsolidationResult parses the DTC output and applies profile/personality
// updates. Returns the updated profile string (or "" if unchanged) and count.
func applyConsolidationResult(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel, raw, currentProfile string, memCount int) (string, int, error) {
	// Try dual output format (profile_updates or user_profile + personality_updates).
	var dual consolidationOutput
	if err := json.Unmarshal([]byte(raw), &dual); err == nil && len(dual.profileData()) > 0 {
		n, err := applyConsolidationDual(ctx, pool, embedEndpoint, embedModel, dual, memCount, currentProfile)
		if err != nil {
			return "", 0, err
		}
		// Read back the updated profile for the next batch.
		updated, rErr := memory.GetIdentityProfile(ctx, pool)
		if rErr != nil {
			updated = currentProfile
		}
		return updated, n, nil
	}

	// Fallback: treat entire output as profile updates (old behavior).
	var fallback map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fallback); err != nil {
		return "", 0, fmt.Errorf("invalid JSON from deep thinker: %w", err)
	}

	hasProfileKey := false
	for _, k := range []string{"name", "important_people", "recurring_topics", "user_preferences"} {
		if _, ok := fallback[k]; ok {
			hasProfileKey = true
			break
		}
	}
	if !hasProfileKey {
		logger.Log.Warnf("[consolidate] deep thinker returned non-profile JSON, discarding: %s", raw)
		return "", 0, nil
	}

	merged, err := mergeProfileJSON(currentProfile, raw)
	if err != nil {
		return "", 0, fmt.Errorf("merge profile: %w", err)
	}

	var mergedMap map[string]any
	if err := json.Unmarshal([]byte(merged), &mergedMap); err != nil {
		return "", 0, fmt.Errorf("parse merged profile: %w", err)
	}
	if err := validateProfile(mergedMap); err != nil {
		logger.Log.Warnf("[consolidate] merged profile failed validation, discarding: %v", err)
		return "", 0, nil
	}

	if strings.TrimSpace(merged) != strings.TrimSpace(currentProfile) {
		if err := memory.WriteIdentityProfile(ctx, pool, embedEndpoint, embedModel, merged); err != nil {
			return "", 0, fmt.Errorf("write updated profile: %w", err)
		}
		return merged, memCount, nil
	}

	return "", memCount, nil
}

// applyConsolidationDual merges profile updates and applies personality updates.
func applyConsolidationDual(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel string, dual consolidationOutput, memCount int, currentProfile string) (int, error) {
	// Merge profile updates into current profile.
	updates := string(dual.profileData())
	merged, err := mergeProfileJSON(currentProfile, updates)
	if err != nil {
		return 0, fmt.Errorf("merge profile: %w", err)
	}

	var mergedMap map[string]any
	if err := json.Unmarshal([]byte(merged), &mergedMap); err != nil {
		return 0, fmt.Errorf("parse merged profile: %w", err)
	}
	if err := validateProfile(mergedMap); err != nil {
		logger.Log.Warnf("[consolidate] merged profile failed validation, discarding: %v", err)
		return 0, nil
	}

	// Avoid rewriting if the profile hasn't changed.
	if strings.TrimSpace(merged) != strings.TrimSpace(currentProfile) {
		if err := memory.WriteIdentityProfile(ctx, pool, embedEndpoint, embedModel, merged); err != nil {
			return 0, fmt.Errorf("write updated profile: %w", err)
		}
	}

	// Apply personality updates.
	for _, u := range dual.PersonalityUpdates {
		switch strings.ToLower(u.Action) {
		case "set":
			if u.Category == "" || u.Key == "" || u.Value == "" {
				continue
			}
			if _, err := memory.UpsertPersonalityTrait(ctx, pool, u.Category, u.Key, u.Value, u.Context); err != nil {
				logger.Log.Warnf("[consolidate] failed to upsert trait %s/%s: %v", u.Category, u.Key, err)
			}
		case "remove":
			if u.Category == "" || u.Key == "" {
				continue
			}
			if _, err := memory.DeletePersonalityTrait(ctx, pool, u.Category, u.Key); err != nil {
				logger.Log.Warnf("[consolidate] failed to delete trait %s/%s: %v", u.Category, u.Key, err)
			}
		}
	}

	if len(dual.PersonalityUpdates) > 0 {
		logger.Log.Infof("[consolidate] applied %d personality updates", len(dual.PersonalityUpdates))
	}

	return memCount, nil
}

// NewConsolidateMemory returns a ToolFunc that triggers the memory consolidation
// pipeline: query high-salience memories from pgvector, read the current identity
// profile from the database, send both to the Deep Thinker, and write the updated
// profile back as an identity memory row.
func NewConsolidateMemory(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, memoryLimit int, refreshFn func()) ToolFunc {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		if pool == nil {
			return "Memory consolidation unavailable: no database configured.", nil
		}
		if dtc == nil {
			return "Memory consolidation unavailable: no deep thinker configured.", nil
		}

		n, err := ConsolidateCore(ctx, pool, dtc, embedEndpoint, embedModel, ConsolidateOpts{
			SalienceThreshold: salienceThreshold,
			MemoryLimit:       memoryLimit,
		})
		if err != nil {
			return fmt.Sprintf("Consolidation failed: %v", err), nil
		}
		if n == 0 {
			return "No high-salience memories (score 8+) found in the last 24 hours. No consolidation needed.", nil
		}

		if refreshFn != nil {
			refreshFn()
		}

		logger.Log.Infof("[consolidate] profile updated from %d high-salience memories", n)
		return fmt.Sprintf("Memory consolidation complete. Synthesized %d high-salience memories into core profile.", n), nil
	}
}

// RunInitialConsolidation runs a one-shot memory consolidation at startup.
// It is intended to be called as a fire-and-forget goroutine so the identity
// profile exists as early as possible.
func RunInitialConsolidation(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, memoryLimit int) {
	logger.Log.Info("[consolidate] running initial profile consolidation on startup")

	ctx, cancel := context.WithTimeout(context.Background(), TimeoutInitConsolidation)
	defer cancel()

	// Use a lower threshold (5) than the regular consolidation (8) so the
	// initial profile can incorporate all available memories, including
	// freshly backfilled emails and conversations.
	n, err := ConsolidateCore(ctx, pool, dtc, embedEndpoint, embedModel, ConsolidateOpts{
		SalienceThreshold: 5,
		MemoryLimit:       memoryLimit,
		Timeout:           TimeoutInitConsolidation,
	})
	if err != nil {
		logger.Log.Errorf("[consolidate] startup: %v", err)
		return
	}
	if n == 0 {
		logger.Log.Info("[consolidate] startup: no memories found, skipping")
		return
	}

	logger.Log.Infof("[consolidate] startup: profile created/updated from %d high-salience memories", n)
}
