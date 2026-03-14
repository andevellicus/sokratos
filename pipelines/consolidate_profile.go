package pipelines

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

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

// toObjectSlice converts an any to []map[string]any. Handles both []any
// (from JSON unmarshal) and []map[string]any (from mergeObjectArray).
func toObjectSlice(v any) []map[string]any {
	if v == nil {
		return nil
	}
	// Direct type — returned by mergeObjectArray.
	if typed, ok := v.([]map[string]any); ok {
		return typed
	}
	// JSON unmarshal produces []any.
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
