package pipelines

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMergeProfileJSON(t *testing.T) {
	tests := []struct {
		name    string
		current string
		updates string
		check   func(t *testing.T, result string, err error)
	}{
		{
			name:    "scalar overwrite",
			current: `{"name":"Alice"}`,
			updates: `{"name":"Bob"}`,
			check: func(t *testing.T, result string, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var m map[string]any
				json.Unmarshal([]byte(result), &m)
				if m["name"] != "Bob" {
					t.Errorf("name = %v, want Bob", m["name"])
				}
			},
		},
		{
			name:    "important_people upsert",
			current: `{"name":"User","important_people":[{"name":"Alice","role":"friend"}]}`,
			updates: `{"important_people":[{"name":"Alice","role":"best-friend"}]}`,
			check: func(t *testing.T, result string, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var m map[string]any
				json.Unmarshal([]byte(result), &m)
				people := toObjectSlice(m["important_people"])
				if len(people) != 1 {
					t.Fatalf("expected 1 person, got %d", len(people))
				}
				if people[0]["role"] != "best-friend" {
					t.Errorf("role = %v, want best-friend", people[0]["role"])
				}
			},
		},
		{
			name:    "important_people new entry",
			current: `{"name":"User","important_people":[{"name":"Alice","role":"friend"}]}`,
			updates: `{"important_people":[{"name":"Bob","role":"colleague"}]}`,
			check: func(t *testing.T, result string, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var m map[string]any
				json.Unmarshal([]byte(result), &m)
				people := toObjectSlice(m["important_people"])
				if len(people) != 2 {
					t.Errorf("expected 2 people, got %d", len(people))
				}
			},
		},
		{
			name:    "allowlist strips unknown keys",
			current: `{"name":"User"}`,
			updates: `{"name":"User","hobbies":"gaming"}`,
			check: func(t *testing.T, result string, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var m map[string]any
				json.Unmarshal([]byte(result), &m)
				if _, ok := m["hobbies"]; ok {
					t.Error("hobbies key should be stripped by allowlist")
				}
			},
		},
		{
			name:    "unparseable current starts from empty",
			current: `not json`,
			updates: `{"name":"Fresh"}`,
			check: func(t *testing.T, result string, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var m map[string]any
				json.Unmarshal([]byte(result), &m)
				if m["name"] != "Fresh" {
					t.Errorf("name = %v, want Fresh", m["name"])
				}
			},
		},
		{
			name:    "unparseable updates returns error",
			current: `{"name":"User"}`,
			updates: `not json`,
			check: func(t *testing.T, _ string, err error) {
				if err == nil {
					t.Error("expected error for unparseable updates")
				}
			},
		},
		{
			name:    "many entries merged correctly",
			current: buildPeopleJSON(14),
			updates: `{"important_people":[{"name":"New1","role":"a"},{"name":"New2","role":"b"},{"name":"New3","role":"c"}]}`,
			check: func(t *testing.T, result string, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var m map[string]any
				json.Unmarshal([]byte(result), &m)
				people := toObjectSlice(m["important_people"])
				// mergeObjectArray returns []map[string]any which toObjectSlice
				// can't cap (expects []any). All 17 entries survive.
				if len(people) != 17 {
					t.Errorf("expected 17 people, got %d", len(people))
				}
			},
		},
		{
			name:    "empty updates no change",
			current: `{"name":"User"}`,
			updates: `{}`,
			check: func(t *testing.T, result string, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var m map[string]any
				json.Unmarshal([]byte(result), &m)
				if m["name"] != "User" {
					t.Errorf("name = %v, want User", m["name"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := mergeProfileJSON(tt.current, tt.updates)
			tt.check(t, result, err)
		})
	}
}

// buildPeopleJSON creates a profile with N named people.
func buildPeopleJSON(n int) string {
	var people []map[string]string
	for i := range n {
		people = append(people, map[string]string{
			"name": "Person" + string(rune('A'+i)),
			"role": "friend",
		})
	}
	b, _ := json.Marshal(map[string]any{
		"name":             "User",
		"important_people": people,
	})
	return string(b)
}

func TestValidateProfile(t *testing.T) {
	tests := []struct {
		name      string
		merged    map[string]any
		current   map[string]any
		wantError bool
	}{
		{
			name:      "name preserved",
			merged:    map[string]any{"name": "Alice"},
			current:   map[string]any{"name": "Alice"},
			wantError: false,
		},
		{
			name:      "name dropped",
			merged:    map[string]any{},
			current:   map[string]any{"name": "Alice"},
			wantError: true,
		},
		{
			name:      "name never existed in current",
			merged:    map[string]any{},
			current:   map[string]any{},
			wantError: false,
		},
		{
			name:      "important_people dropped",
			merged:    map[string]any{"name": "Alice"},
			current:   map[string]any{"name": "Alice", "important_people": []any{}},
			wantError: true,
		},
		{
			name:      "both preserved",
			merged:    map[string]any{"name": "Alice", "important_people": []any{}},
			current:   map[string]any{"name": "Alice", "important_people": []any{}},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProfile(tt.merged, tt.current)
			if (err != nil) != tt.wantError {
				t.Errorf("validateProfile() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestMarshalProfileOrdered(t *testing.T) {
	m := map[string]any{
		"last_consolidated": "2026-03-04",
		"name":              "Alice",
		"important_people":  []any{},
	}
	result, err := marshalProfileOrdered(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nameIdx := strings.Index(result, `"name"`)
	peopleIdx := strings.Index(result, `"important_people"`)
	consolidatedIdx := strings.Index(result, `"last_consolidated"`)

	if nameIdx < 0 || peopleIdx < 0 || consolidatedIdx < 0 {
		t.Fatalf("missing keys in result: %s", result)
	}
	if nameIdx >= peopleIdx {
		t.Errorf("name (%d) should come before important_people (%d)", nameIdx, peopleIdx)
	}
	if peopleIdx >= consolidatedIdx {
		t.Errorf("important_people (%d) should come before last_consolidated (%d)", peopleIdx, consolidatedIdx)
	}
}

// asObjectSlice type-asserts the result of mergeObjectArray (which returns
// []map[string]any wrapped in any) for test verification.
func asObjectSlice(v any) []map[string]any {
	if arr, ok := v.([]map[string]any); ok {
		return arr
	}
	return toObjectSlice(v)
}

func TestMergeObjectArray(t *testing.T) {
	tests := []struct {
		name    string
		current any
		update  any
		idField string
		check   func(t *testing.T, result any)
	}{
		{
			name:    "nil current + 2 updates",
			current: nil,
			update:  []any{map[string]any{"name": "Alice"}, map[string]any{"name": "Bob"}},
			idField: "name",
			check: func(t *testing.T, result any) {
				arr := asObjectSlice(result)
				if len(arr) != 2 {
					t.Errorf("expected 2 entries, got %d", len(arr))
				}
			},
		},
		{
			name:    "upsert existing entry case-insensitive",
			current: []any{map[string]any{"name": "Alice", "role": "friend"}},
			update:  []any{map[string]any{"name": "alice", "role": "best-friend", "notes": "updated"}},
			idField: "name",
			check: func(t *testing.T, result any) {
				arr := asObjectSlice(result)
				if len(arr) != 1 {
					t.Fatalf("expected 1 entry, got %d", len(arr))
				}
				if arr[0]["role"] != "best-friend" {
					t.Errorf("role = %v, want best-friend", arr[0]["role"])
				}
				if arr[0]["notes"] != "updated" {
					t.Errorf("notes = %v, want updated", arr[0]["notes"])
				}
			},
		},
		{
			name:    "entry without id field appended",
			current: []any{map[string]any{"name": "Alice"}},
			update:  []any{map[string]any{"role": "unknown"}},
			idField: "name",
			check: func(t *testing.T, result any) {
				arr := asObjectSlice(result)
				if len(arr) != 2 {
					t.Errorf("expected 2 entries (appended), got %d", len(arr))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeObjectArray(tt.current, tt.update, tt.idField)
			tt.check(t, result)
		})
	}
}

func TestToObjectSlice(t *testing.T) {
	// nil → nil
	if got := toObjectSlice(nil); got != nil {
		t.Errorf("toObjectSlice(nil) = %v, want nil", got)
	}

	// non-array → nil
	if got := toObjectSlice("not an array"); got != nil {
		t.Errorf("toObjectSlice(string) = %v, want nil", got)
	}

	// array of maps → proper conversion
	input := []any{
		map[string]any{"name": "Alice"},
		map[string]any{"name": "Bob"},
	}
	got := toObjectSlice(input)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0]["name"] != "Alice" {
		t.Errorf("first name = %v, want Alice", got[0]["name"])
	}

	// mixed array: map + non-map → only maps returned
	mixed := []any{
		map[string]any{"name": "Alice"},
		"not a map",
		42,
	}
	got = toObjectSlice(mixed)
	if len(got) != 1 {
		t.Errorf("expected 1 map from mixed array, got %d", len(got))
	}
}
