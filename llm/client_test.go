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

func TestExtractToolIntent(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantOK  bool
	}{
		{
			name:   "clean intent",
			input:  `Some prose <TOOL_INTENT>search_web: {"query": "test"}</TOOL_INTENT> more text`,
			want:   `search_web: {"query": "test"}`,
			wantOK: true,
		},
		{
			name:   "CODE block intent",
			input:  `<TOOL_INTENT>create_skill: {"name":"s"}<CODE>console.log('hi')</CODE></TOOL_INTENT>`,
			want:   `create_skill: {"name":"s"}<CODE>console.log('hi')</CODE>`,
			wantOK: true,
		},
		{
			name:   "unclosed tag returns false",
			input:  `<TOOL_INTENT>search_web: {"query": "test"}`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "opening tag only no content",
			input:  `Just mentioning <TOOL_INTENT> in prose`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "intent with surrounding prose",
			input:  `Let me save this. <TOOL_INTENT>save_memory: {"summary":"x"}</TOOL_INTENT> Done.`,
			want:   `save_memory: {"summary":"x"}`,
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractToolIntent(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("extractToolIntent() ok = %v, wantOK %v (got %q)", ok, tt.wantOK, got)
			}
			if got != tt.want {
				t.Errorf("extractToolIntent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractBareToolTag(t *testing.T) {
	// Mock tool checker that recognizes common tools.
	knownTools := map[string]bool{
		"run_command":     true,
		"search_web":     true,
		"search_memory":  true,
		"save_memory":    true,
		"search_email":   true,
		"search_calendar": true,
		"get_weather":    true,
	}
	isKnown := func(name string) bool { return knownTools[name] }

	tests := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{
			name:   "run_command with key:value",
			input:  `<run_command>command: ls -t ~/code/go/tychora/logs | head -n 1</run_command>`,
			want:   `run_command: {"command": "ls -t ~/code/go/tychora/logs | head -n 1"}`,
			wantOK: true,
		},
		{
			name:   "search_web with JSON content",
			input:  `<search_web>{"query": "golang testing"}</search_web>`,
			want:   `search_web: {"query": "golang testing"}`,
			wantOK: true,
		},
		{
			name:   "non-tool tag rejected by blocklist",
			input:  `<think>I should search for this</think>`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "memory tag rejected by blocklist",
			input:  `<memory>some content</memory>`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "uppercase tag not matched by regex",
			input:  `<TOOL_INTENT>search_web: {}</TOOL_INTENT>`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "surrounding prose",
			input:  `Let me check. <run_command>command: ls</run_command> Done.`,
			want:   `run_command: {"command": "ls"}`,
			wantOK: true,
		},
		{
			name:   "no content or colon",
			input:  `<run_command>no colon here</run_command>`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "unknown tool rejected by registry check",
			input:  `<unknown_tool>arg: value</unknown_tool>`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "nil isKnownTool still blocks non-tool tags",
			input:  `<think>reasoning</think>`,
			want:   "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := isKnown
			if tt.name == "nil isKnownTool still blocks non-tool tags" {
				checker = nil
			}
			got, ok := extractBareToolTag(tt.input, checker)
			if ok != tt.wantOK {
				t.Fatalf("extractBareToolTag() ok = %v, wantOK %v (got %q)", ok, tt.wantOK, got)
			}
			if got != tt.want {
				t.Errorf("extractBareToolTag() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSupervisorParser_BareTag(t *testing.T) {
	// End-to-end: SupervisorParser with IsKnownTool should parse a bare tool tag.
	isKnown := func(name string) bool { return name == "run_command" }
	p := SupervisorParser{IsKnownTool: isKnown}
	pr := p.Parse(`<run_command>command: echo hello</run_command>`)
	if !pr.Found {
		t.Fatal("expected Found=true")
	}
	if pr.Error != "" {
		t.Fatalf("unexpected error: %s", pr.Error)
	}
	if pr.ToolCallJSON == "" {
		t.Fatal("expected non-empty ToolCallJSON")
	}

	var tc struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(pr.ToolCallJSON), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.Name != "run_command" {
		t.Errorf("expected name=run_command, got %q", tc.Name)
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args.Command != "echo hello" {
		t.Errorf("expected command='echo hello', got %q", args.Command)
	}
}

func TestSupervisorParser_BareTagIgnoredWithoutChecker(t *testing.T) {
	// Without IsKnownTool, bare tags should NOT be recovered.
	p := SupervisorParser{}
	pr := p.Parse(`<run_command>command: echo hello</run_command>`)
	if pr.Found {
		t.Error("expected Found=false without IsKnownTool")
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
