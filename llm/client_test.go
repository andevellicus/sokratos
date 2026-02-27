package llm

import (
	"encoding/json"
	"testing"
)

func TestParseToolIntent_SimpleArgs(t *testing.T) {
	toolJSON, ok := parseToolIntent(`search_web: {"query": "weather in SC"}`)
	if !ok {
		t.Fatalf("expected success, got error: %s", toolJSON)
	}

	var tc struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(toolJSON), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.Name != "search_web" {
		t.Errorf("expected name=search_web, got %q", tc.Name)
	}

	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args.Query != "weather in SC" {
		t.Errorf("expected query='weather in SC', got %q", args.Query)
	}
}

func TestParseToolIntent_EmptyArgs(t *testing.T) {
	toolJSON, ok := parseToolIntent(`check_email: {}`)
	if !ok {
		t.Fatalf("expected success, got error: %s", toolJSON)
	}

	var tc struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(toolJSON), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.Name != "check_email" {
		t.Errorf("expected name=check_email, got %q", tc.Name)
	}
	if string(tc.Arguments) != "{}" {
		t.Errorf("expected empty args, got %s", tc.Arguments)
	}
}

func TestParseToolIntent_ComplexArgs(t *testing.T) {
	// Simulates a create_skill intent with escaped JSON in params field.
	intent := `create_skill: {"name": "get-weather", "description": "Fetches weather", "params": "[{\"Name\":\"location\",\"Type\":\"string\",\"Required\":true}]", "code": "var x = 1 + 2; x;"}`
	toolJSON, ok := parseToolIntent(intent)
	if !ok {
		t.Fatalf("expected success, got error: %s", toolJSON)
	}

	var tc struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(toolJSON), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.Name != "create_skill" {
		t.Errorf("expected name=create_skill, got %q", tc.Name)
	}

	// Verify that the params field is preserved as a string (not deserialized into an array).
	var args struct {
		Name   string `json:"name"`
		Params string `json:"params"`
	}
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args.Params != `[{"Name":"location","Type":"string","Required":true}]` {
		t.Errorf("params was corrupted: %q", args.Params)
	}
}

func TestParseToolIntent_InvalidJSON(t *testing.T) {
	_, ok := parseToolIntent(`search_web: not json`)
	if ok {
		t.Error("expected failure for invalid JSON")
	}
}

func TestParseToolIntent_NoColon(t *testing.T) {
	_, ok := parseToolIntent(`check_email {}`)
	if ok {
		t.Error("expected failure for missing colon")
	}
}

func TestParseToolIntent_PreservesArgumentTypes(t *testing.T) {
	// The key property: parseToolIntent uses json.RawMessage so it
	// never deserializes/re-serializes the args. Whatever JSON the
	// orchestrator wrote is passed through byte-for-byte.
	intent := `save_memory: {"summary": "test", "salience_score": 7, "tags": ["a", "b"]}`
	toolJSON, ok := parseToolIntent(intent)
	if !ok {
		t.Fatalf("expected success, got error: %s", toolJSON)
	}

	var tc struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	json.Unmarshal([]byte(toolJSON), &tc)

	var args struct {
		SalienceScore float64  `json:"salience_score"`
		Tags          []string `json:"tags"`
	}
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args.SalienceScore != 7 {
		t.Errorf("expected salience_score=7, got %v", args.SalienceScore)
	}
	if len(args.Tags) != 2 || args.Tags[0] != "a" {
		t.Errorf("expected tags=[a,b], got %v", args.Tags)
	}
}
