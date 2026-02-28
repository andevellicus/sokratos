package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timefmt"
)

const salienceThreshold = 8

// validateProfile checks that essential identity card keys survived the merge.
// Only validates that "name" and "important_people" are preserved if they
// existed before (the allowlist filter intentionally strips non-card fields).
func validateProfile(mergedMap, currentMap map[string]any) error {
	for _, key := range []string{"name", "important_people"} {
		if _, existed := currentMap[key]; existed {
			if _, ok := mergedMap[key]; !ok {
				return fmt.Errorf("merge dropped essential key %q", key)
			}
		}
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
	}

	for key, updVal := range updatesMap {
		if idField, ok := objectArrayIDs[key]; ok {
			currentMap[key] = mergeObjectArray(currentMap[key], updVal, idField)
		} else {
			// Scalar or unknown type: overwrite.
			currentMap[key] = updVal
		}
	}

	// Allowlist: strip any key not in the identity card schema.
	cardKeys := map[string]bool{"name": true, "important_people": true, "last_consolidated": true}
	for k := range currentMap {
		if !cardKeys[k] {
			delete(currentMap, k)
		}
	}

	// Cap important_people at 15 entries (keep tail — mergeObjectArray appends new).
	if people := toObjectSlice(currentMap["important_people"]); len(people) > 15 {
		trimmed := make([]any, 15)
		for i, p := range people[len(people)-15:] {
			trimmed[i] = p
		}
		currentMap["important_people"] = trimmed
	}

	return marshalProfileOrdered(currentMap)
}

// profileKeyOrder defines the canonical key order for identity card profiles.
var profileKeyOrder = []string{
	"name",
	"important_people",
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

// ConsolidateOpts configures a consolidation run.
type ConsolidateOpts struct {
	SalienceThreshold int
	MemoryLimit       int
	Timeout           time.Duration
	MaxPromptChars    int // soft cap on user-content chars to avoid exceeding model context; 0 = 14000
}

// memoryMerge describes a group of redundant memories to merge.
type memoryMerge struct {
	SourceIDs     []int  `json:"source_ids"`
	MergedSummary string `json:"merged_summary"`
}

// consolidationOutput is the expected dual structure from the consolidation prompt.
type consolidationOutput struct {
	ProfileUpdates     json.RawMessage     `json:"profile_updates"`
	UserProfile        json.RawMessage     `json:"user_profile"` // legacy fallback
	PersonalityUpdates []personalityUpdate `json:"personality_updates"`
	MemoryMerges       []memoryMerge       `json:"memory_merges"`
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
func ConsolidateCore(ctx context.Context, pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, opts ConsolidateOpts, grammarFn memory.GrammarSubagentFunc) (int, error) {
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

		// Fill this batch up to the memory budget, tracking IDs for merge validation.
		included := 0
		used := 0
		var batchIDs []int
		for _, m := range remaining {
			line := fmt.Sprintf("[id=%d] %s\n", m.ID, m.Summary)
			if used+len(line) > memBudget && included > 0 {
				break
			}
			fixedPrompt.WriteString(line)
			used += len(line)
			batchIDs = append(batchIDs, m.ID)
			included++
		}
		remaining = remaining[included:]

		if batch > 1 || len(remaining) > 0 {
			logger.Log.Infof("[consolidate] batch %d: processing %d memories (%d remaining)", batch, included, len(remaining))
		}

		var reqCtx context.Context
		var cancel context.CancelFunc
		if opts.Timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		} else {
			reqCtx, cancel = context.WithTimeout(ctx, 10*time.Minute)
		}
		raw, cErr := dtc.CompleteNoThink(reqCtx, strings.TrimSpace(prompts.Consolidation), fixedPrompt.String(), 4096)
		cancel()
		if cErr != nil {
			return totalProcessed, fmt.Errorf("consolidation request (batch %d): %w", batch, cErr)
		}

		raw = textutil.CleanLLMJSON(raw)

		updatedProfile, n, aErr := applyConsolidationResult(ctx, pool, embedEndpoint, embedModel, raw, currentProfile, included, batchIDs, grammarFn)
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
func buildConsolidationPrompt(currentProfile string, traits []memory.PersonalityTrait) *strings.Builder {
	b := &strings.Builder{}
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
	fmt.Fprintf(b, "(Current time: %s)\n\n", timefmt.Now())
	return b
}

// applyConsolidationResult parses the DTC output and applies profile/personality
// updates and memory merges. Returns the updated profile string (or "" if unchanged) and count.
func applyConsolidationResult(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel, raw, currentProfile string, memCount int, batchIDs []int, grammarFn memory.GrammarSubagentFunc) (string, int, error) {
	// Parse current profile once for validation comparisons.
	var currentMap map[string]any
	if err := json.Unmarshal([]byte(currentProfile), &currentMap); err != nil {
		currentMap = make(map[string]any)
	}

	// Try dual output format (profile_updates or user_profile + personality_updates).
	var dual consolidationOutput
	if err := json.Unmarshal([]byte(raw), &dual); err == nil && len(dual.profileData()) > 0 {
		n, err := applyConsolidationDual(ctx, pool, embedEndpoint, embedModel, dual, memCount, currentProfile, currentMap, batchIDs, grammarFn)
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
	for _, k := range []string{"name", "important_people"} {
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
	if err := validateProfile(mergedMap, currentMap); err != nil {
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

// applyConsolidationDual merges profile updates, applies personality updates,
// and executes memory merges.
func applyConsolidationDual(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel string, dual consolidationOutput, memCount int, currentProfile string, currentMap map[string]any, batchIDs []int, grammarFn memory.GrammarSubagentFunc) (int, error) {
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
	if err := validateProfile(mergedMap, currentMap); err != nil {
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

	// Apply memory merges.
	if len(dual.MemoryMerges) > 0 {
		mergedCount, mErr := applyMemoryMerges(ctx, pool, embedEndpoint, embedModel, dual.MemoryMerges, batchIDs, grammarFn)
		if mErr != nil {
			logger.Log.Warnf("[consolidate] memory merge error: %v", mErr)
		} else if mergedCount > 0 {
			logger.Log.Infof("[consolidate] merged %d memories into %d new entries", mergedCount, len(dual.MemoryMerges))
		}
	}

	return memCount, nil
}

// applyMemoryMerges creates merged memories and supersedes the originals.
// Only processes merges where all source_ids are in the current batch.
func applyMemoryMerges(ctx context.Context, pool *pgxpool.Pool, embedEndpoint, embedModel string, merges []memoryMerge, batchIDs []int, grammarFn memory.GrammarSubagentFunc) (int, error) {
	// Build lookup set for batch validation.
	validIDs := make(map[int]bool, len(batchIDs))
	for _, id := range batchIDs {
		validIDs[id] = true
	}

	merged := 0
	for _, m := range merges {
		if len(m.SourceIDs) < 2 || strings.TrimSpace(m.MergedSummary) == "" {
			continue
		}

		// Validate all source IDs belong to the current batch.
		valid := true
		for _, id := range m.SourceIDs {
			if !validIDs[id] {
				logger.Log.Warnf("[consolidate] merge skipped: source_id %d not in current batch", id)
				valid = false
				break
			}
		}
		if !valid {
			continue
		}

		// Get max salience from source memories.
		var maxSalience float64
		err := pool.QueryRow(ctx,
			`SELECT COALESCE(MAX(salience), 5) FROM memories WHERE id = ANY($1)`,
			m.SourceIDs,
		).Scan(&maxSalience)
		if err != nil {
			logger.Log.Warnf("[consolidate] merge: failed to query salience: %v", err)
			continue
		}

		// Embed the merged summary.
		vec, err := memory.GetEmbedding(ctx, embedEndpoint, embedModel, m.MergedSummary)
		if err != nil {
			logger.Log.Warnf("[consolidate] merge: failed to embed: %v", err)
			continue
		}

		// Transaction: insert merged memory, supersede sources.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return merged, fmt.Errorf("begin merge tx: %w", err)
		}

		var newID int
		err = tx.QueryRow(ctx,
			`INSERT INTO memories (summary, embedding, salience, memory_type, source, entities)
			 VALUES ($1, $2, $3, 'general', 'consolidation', $4)
			 RETURNING id`,
			m.MergedSummary, pgvector.NewVector(vec), maxSalience, []string{},
		).Scan(&newID)
		if err != nil {
			tx.Rollback(ctx)
			logger.Log.Warnf("[consolidate] merge: failed to insert: %v", err)
			continue
		}

		_, err = tx.Exec(ctx,
			`UPDATE memories SET superseded_by = $1 WHERE id = ANY($2) AND superseded_by IS NULL`,
			newID, m.SourceIDs,
		)
		if err != nil {
			tx.Rollback(ctx)
			logger.Log.Warnf("[consolidate] merge: failed to supersede: %v", err)
			continue
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Log.Warnf("[consolidate] merge: commit failed: %v", err)
			continue
		}

		logger.Log.Infof("[consolidate] merged ids %v → id=%d: %s", m.SourceIDs, newID, textutil.Truncate(m.MergedSummary, 100))

		// Fire async entity enrichment for the merged memory.
		if grammarFn != nil {
			go memory.EnrichViaGrammarFn(pool, grammarFn, []int64{int64(newID)}, m.MergedSummary, maxSalience, nil)
		}

		merged += len(m.SourceIDs)
	}

	return merged, nil
}

// NewConsolidateMemory returns a ToolFunc that triggers the memory consolidation
// pipeline: query high-salience memories from pgvector, read the current identity
// profile from the database, send both to the Deep Thinker, and write the updated
// profile back as an identity memory row.
func NewConsolidateMemory(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, memoryLimit int, grammarFn memory.GrammarSubagentFunc, refreshFn func()) ToolFunc {
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
		}, grammarFn)
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
func RunInitialConsolidation(pool *pgxpool.Pool, dtc *DeepThinkerClient, embedEndpoint, embedModel string, memoryLimit int, grammarFn memory.GrammarSubagentFunc) {
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
	}, grammarFn)
	if err != nil {
		logger.Log.Errorf("[consolidate] startup: %v", err)
		return
	}
	if n == 0 {
		logger.Log.Info("[consolidate] startup: no actionable updates produced, skipping")
		return
	}

	logger.Log.Infof("[consolidate] startup: profile created/updated from %d high-salience memories", n)
}
