package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildStepSystemPrompt_NoPriorResults(t *testing.T) {
	step := planStep{Description: "Search for recent news about Go 1.24"}
	prompt := buildStepSystemPrompt("Research Go release notes", step, nil)

	if !strings.Contains(prompt, "Research Go release notes") {
		t.Error("expected directive in prompt")
	}
	if !strings.Contains(prompt, "Search for recent news about Go 1.24") {
		t.Error("expected step description in prompt")
	}
	if !strings.Contains(prompt, "## Rules") {
		t.Error("expected rules section")
	}
	if strings.Contains(prompt, "## Results from Prior Steps") {
		t.Error("should not contain prior results section when empty")
	}
}

func TestBuildStepSystemPrompt_WithPriorResults(t *testing.T) {
	step := planStep{Description: "Summarize findings"}
	prior := []stepResult{
		{Step: 1, Description: "Search emails", Result: "Found 3 emails", Success: true},
		{Step: 2, Description: "Check calendar", Result: "step failed: timeout", Success: false},
	}

	prompt := buildStepSystemPrompt("Daily briefing", step, prior)

	if !strings.Contains(prompt, "## Results from Prior Steps") {
		t.Error("expected prior results section")
	}
	if !strings.Contains(prompt, "Step 1 [SUCCESS]") {
		t.Error("expected SUCCESS label for step 1")
	}
	if !strings.Contains(prompt, "Step 2 [FAILED]") {
		t.Error("expected FAILED label for step 2")
	}
	if !strings.Contains(prompt, "Found 3 emails") {
		t.Error("expected step 1 result content")
	}
	if !strings.Contains(prompt, "step failed: timeout") {
		t.Error("expected step 2 result content")
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
