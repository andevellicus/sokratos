package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildStepSystemPrompt_NoPriorResults(t *testing.T) {
	step := planStep{Description: "Search for recent news about Go 1.24"}
	pad := &Scratchpad{}
	prompt := buildStepSystemPrompt("Research Go release notes", step, pad)

	if !strings.Contains(prompt, "Research Go release notes") {
		t.Error("expected directive in prompt")
	}
	if !strings.Contains(prompt, "Search for recent news about Go 1.24") {
		t.Error("expected step description in prompt")
	}
	if !strings.Contains(prompt, "## Rules") {
		t.Error("expected rules section")
	}
	if strings.Contains(prompt, "## Context from Prior Steps") {
		t.Error("should not contain context section when scratchpad is empty")
	}
}

func TestBuildStepSystemPrompt_WithScratchpad(t *testing.T) {
	step := planStep{Description: "Summarize findings"}
	pad := &Scratchpad{}
	pad.Set("step_1", "Found 3 emails about Go releases")
	pad.Set("step_2", "step failed: timeout")
	pad.Set("failures", "Step 2 failed: timeout")

	prompt := buildStepSystemPrompt("Daily briefing", step, pad)

	if !strings.Contains(prompt, "## Context from Prior Steps") {
		t.Error("expected context section")
	}
	if !strings.Contains(prompt, "step_1: Found 3 emails") {
		t.Error("expected step 1 result content")
	}
	if !strings.Contains(prompt, "failures: Step 2 failed: timeout") {
		t.Error("expected failures entry")
	}
}

func TestScratchpad_SetGetTruncate(t *testing.T) {
	pad := &Scratchpad{}
	pad.Set("key1", "value1")
	if got := pad.Get("key1"); got != "value1" {
		t.Errorf("expected value1, got %q", got)
	}
	if got := pad.Get("missing"); got != "" {
		t.Errorf("expected empty for missing key, got %q", got)
	}

	// Test overwrite.
	pad.Set("key1", "updated")
	if got := pad.Get("key1"); got != "updated" {
		t.Errorf("expected updated, got %q", got)
	}

	// Test truncation (scratchpadBudget = 1500).
	long := strings.Repeat("x", 2000)
	pad.Set("long", long)
	got := pad.Get("long")
	if len(got) > scratchpadBudget+3 { // +3 for "..."
		t.Errorf("expected truncation, got len=%d", len(got))
	}
}

func TestScratchpad_FormatForPrompt(t *testing.T) {
	pad := &Scratchpad{}
	if got := pad.FormatForPrompt(); got != "" {
		t.Errorf("expected empty string for empty scratchpad, got %q", got)
	}

	pad.Set("step_1", "result A")
	pad.Set("step_2", "result B")
	out := pad.FormatForPrompt()
	if !strings.Contains(out, "- step_1: result A") {
		t.Error("expected step_1 entry")
	}
	if !strings.Contains(out, "- step_2: result B") {
		t.Error("expected step_2 entry")
	}
}

func TestIsComplexStep(t *testing.T) {
	cases := []struct {
		name    string
		step    planStep
		complex bool
	}{
		{"simple search", planStep{Description: "Search for emails", ToolsNeeded: []string{"search_email"}}, false},
		{"retrieval only", planStep{Description: "Analyze patterns in emails", ToolsNeeded: []string{"search_email", "search_memory"}}, false},
		{"analyze keyword", planStep{Description: "Analyze the search results", ToolsNeeded: []string{}}, true},
		{"synthesize keyword", planStep{Description: "Synthesize findings from previous steps"}, true},
		{"compare keyword", planStep{Description: "Compare the two approaches"}, true},
		{"consolidate keyword", planStep{Description: "Consolidate all data points"}, true},
		{"no keywords", planStep{Description: "Look up the weather forecast"}, false},
		{"no tools no keywords", planStep{Description: "Find the answer"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isComplexStep(tc.step)
			if got != tc.complex {
				t.Errorf("isComplexStep(%q, tools=%v) = %v, want %v",
					tc.step.Description, tc.step.ToolsNeeded, got, tc.complex)
			}
		})
	}
}

func TestFormatResults_AllSuccess(t *testing.T) {
	results := []stepResult{
		{Step: 1, Description: "Step one", Result: "Done", Success: true},
		{Step: 2, Description: "Step two", Result: "Done too", Success: true},
	}
	out := formatResults(results)

	if !strings.Contains(out, "2/2 steps succeeded") {
		t.Errorf("expected 2/2 succeeded, got: %s", out)
	}
	if !strings.Contains(out, "[OK]") {
		t.Error("expected [OK] markers")
	}
	if strings.Contains(out, "[FAILED]") {
		t.Error("should not contain [FAILED]")
	}
}

func TestFormatResults_MixedResults(t *testing.T) {
	results := []stepResult{
		{Step: 1, Description: "Worked", Result: "ok", Success: true},
		{Step: 2, Description: "Broke", Result: "error", Success: false},
		{Step: 3, Description: "Also worked", Result: "ok", Success: true},
	}
	out := formatResults(results)

	if !strings.Contains(out, "2/3 steps succeeded") {
		t.Errorf("expected 2/3 succeeded, got: %s", out)
	}
	if !strings.Contains(out, "[OK]") {
		t.Error("expected [OK] marker")
	}
	if !strings.Contains(out, "[FAILED]") {
		t.Error("expected [FAILED] marker")
	}
}

func TestFormatResults_AllFailed(t *testing.T) {
	results := []stepResult{
		{Step: 1, Description: "Oops", Result: "crashed", Success: false},
	}
	out := formatResults(results)

	if !strings.Contains(out, "0/1 steps succeeded") {
		t.Errorf("expected 0/1 succeeded, got: %s", out)
	}
}

func TestFormatResults_Empty(t *testing.T) {
	out := formatResults(nil)

	if !strings.Contains(out, "0/0 steps succeeded") {
		t.Errorf("expected 0/0 succeeded, got: %s", out)
	}
}

func TestCheckBackgroundTask_ArgParsing(t *testing.T) {
	// We can't test with a real BackgroundTaskRunner (needs DB), but we
	// can verify the arg parsing and routing logic by inspecting the
	// JSON unmarshalling and action defaults.

	cases := []struct {
		name       string
		args       string
		wantAction string
		wantTaskID int64
	}{
		{
			name:       "list action ignores task_id",
			args:       `{"action": "list"}`,
			wantAction: "list",
			wantTaskID: 0,
		},
		{
			name:       "status with task_id",
			args:       `{"action": "status", "task_id": 42}`,
			wantAction: "status",
			wantTaskID: 42,
		},
		{
			name:       "cancel with task_id",
			args:       `{"action": "cancel", "task_id": 7}`,
			wantAction: "cancel",
			wantTaskID: 7,
		},
		{
			name:       "default action is status",
			args:       `{"task_id": 99}`,
			wantAction: "status",
			wantTaskID: 99,
		},
		{
			name:       "empty args defaults to status",
			args:       `{}`,
			wantAction: "status",
			wantTaskID: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a struct {
				TaskID int64  `json:"task_id"`
				Action string `json:"action"`
			}
			if err := json.Unmarshal([]byte(tc.args), &a); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}

			action := a.Action
			if action == "" {
				action = "status"
			}

			if action != tc.wantAction {
				t.Errorf("expected action=%q, got %q", tc.wantAction, action)
			}
			if a.TaskID != tc.wantTaskID {
				t.Errorf("expected task_id=%d, got %d", tc.wantTaskID, a.TaskID)
			}
		})
	}
}

// TestStripHTML verifies the HTML stripping used by read_url.
func TestStripHTML(t *testing.T) {
	input := `<html><head><script>alert("xss")</script><style>body{color:red}</style></head>
<body><h1>Hello</h1><p>World &amp; friends</p></body></html>`

	got := stripHTML(input)

	if strings.Contains(got, "<") {
		t.Error("expected all HTML tags removed")
	}
	if strings.Contains(got, "alert") {
		t.Error("expected script content removed")
	}
	if strings.Contains(got, "color:red") {
		t.Error("expected style content removed")
	}
	if !strings.Contains(got, "Hello") {
		t.Error("expected text content preserved")
	}
	if !strings.Contains(got, "World & friends") {
		t.Error("expected HTML entities decoded")
	}
}

// Ensure exported function isn't accidentally broken — this is a compile-time check.
var _ = func() {
	_ = NewSearchWeb("http://localhost:9000")
	_ = NewReadURL()
	_ = context.Background // suppress unused import
}
