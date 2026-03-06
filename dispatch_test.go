package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"sokratos/engine"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/tools"
)

func TestMain(m *testing.M) {
	// Initialize logger to avoid nil pointer in Registry.Register.
	logger.Init(os.TempDir())
	os.Exit(m.Run())
}

func TestNeverDispatchTools(t *testing.T) {
	expected := []string{
		"send_email", "create_event", "create_skill", "manage_skills",
		"manage_routines", "manage_personality", "save_memory", "forget_topic",
		"reason", "plan_and_execute", "delegate_task", "ask_database",
		"manage_objectives", "write_file", "patch_file", "update_skill",
		"reply_to_job", "cancel_job", "run_command",
	}
	for _, tool := range expected {
		if !neverDispatchTools[tool] {
			t.Errorf("expected %q in neverDispatchTools", tool)
		}
	}
}

func TestBuildTriageInput(t *testing.T) {
	tests := []struct {
		name     string
		msgText  string
		history  []llm.Message
		contains []string
	}{
		{
			name:    "no history",
			msgText: "What's the weather?",
			contains: []string{
				"New message: What's the weather?",
			},
		},
		{
			name:    "with history",
			msgText: "Tell me more",
			history: []llm.Message{
				{Role: "user", Content: "Hello"},
				{Role: "assistant", Content: "Hi there!"},
			},
			contains: []string{
				"Recent conversation:",
				"user: Hello",
				"assistant: Hi there!",
				"New message: Tell me more",
			},
		},
		{
			name:    "history truncated to 4",
			msgText: "Latest",
			history: []llm.Message{
				{Role: "user", Content: "msg1"},
				{Role: "assistant", Content: "reply1"},
				{Role: "user", Content: "msg2"},
				{Role: "assistant", Content: "reply2"},
				{Role: "user", Content: "msg3"},
				{Role: "assistant", Content: "reply3"},
			},
			contains: []string{
				"user: msg2",       // 3rd from end — included (last 4)
				"assistant: reply3", // last — included
				"New message: Latest",
			},
		},
		{
			name:    "long message truncated",
			msgText: "short",
			history: []llm.Message{
				{Role: "user", Content: strings.Repeat("x", 500)},
			},
			contains: []string{
				"user: " + strings.Repeat("x", 200) + "...", // Truncate(s, 200) = s[:200] + "..."
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := buildTriageInput(tc.msgText, tc.history)
			for _, s := range tc.contains {
				if !strings.Contains(result, s) {
					t.Errorf("expected output to contain %q, got:\n%s", s, result)
				}
			}
		})
	}
}

func TestBuildContextualPrompt(t *testing.T) {
	tests := []struct {
		name         string
		dctx         dispatchContext
		instructions string
		contains     []string
		notContains  []string
	}{
		{
			name:         "instructions only",
			dctx:         dispatchContext{},
			instructions: "Be helpful.",
			contains:     []string{"Be helpful."},
			notContains:  []string{"About the user", "Relevant memories", "Temporal context"},
		},
		{
			name: "all fields populated",
			dctx: dispatchContext{
				PersonalityContent: "You are witty.",
				ProfileContent:     "Works in tech.",
				PrefetchContent:    "Memory about Go.",
				TemporalCtx:        "Thursday 6 March 2026",
			},
			instructions: "Present results naturally.",
			contains: []string{
				"You are witty.",
				"Present results naturally.",
				"## About the user\nWorks in tech.",
				"## Relevant memories\nMemory about Go.",
				"## Temporal context\nThursday 6 March 2026",
			},
		},
		{
			name: "personality before instructions",
			dctx: dispatchContext{
				PersonalityContent: "PERSONALITY",
			},
			instructions: "INSTRUCTIONS",
			contains:     []string{"PERSONALITY\n\nINSTRUCTIONS"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := buildContextualPrompt(tc.dctx, tc.instructions)
			for _, s := range tc.contains {
				if !strings.Contains(result, s) {
					t.Errorf("expected output to contain %q, got:\n%s", s, result)
				}
			}
			for _, s := range tc.notContains {
				if strings.Contains(result, s) {
					t.Errorf("expected output NOT to contain %q, got:\n%s", s, result)
				}
			}
		})
	}
}

func TestDispatchResultParsing(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		dispatch bool
		multi    bool
		tool     string
		ack      string
	}{
		{
			name:     "escalate",
			json:     `{"dispatch": false, "ack": "Let me think about that."}`,
			dispatch: false,
			ack:      "Let me think about that.",
		},
		{
			name:     "single tool dispatch",
			json:     `{"dispatch": true, "tool": "search_web", "args": {"query": "golang"}, "ack": "Sure, checking..."}`,
			dispatch: true,
			tool:     "search_web",
			ack:      "Sure, checking...",
		},
		{
			name:     "multi-step dispatch",
			json:     `{"dispatch": true, "multi": true, "directive": "Search and summarize", "ack": "Working on it..."}`,
			dispatch: true,
			multi:    true,
			ack:      "Working on it...",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dr dispatchResult
			if err := json.Unmarshal([]byte(tc.json), &dr); err != nil {
				t.Fatalf("failed to parse: %v", err)
			}
			if dr.Dispatch != tc.dispatch {
				t.Errorf("dispatch: got %v, want %v", dr.Dispatch, tc.dispatch)
			}
			if dr.Multi != tc.multi {
				t.Errorf("multi: got %v, want %v", dr.Multi, tc.multi)
			}
			if dr.Tool != tc.tool {
				t.Errorf("tool: got %q, want %q", dr.Tool, tc.tool)
			}
			if dr.Ack != tc.ack {
				t.Errorf("ack: got %q, want %q", dr.Ack, tc.ack)
			}
		})
	}
}

func TestBuildTriageSystemPrompt(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register("search_web", func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", nil
	}, tools.ToolSchema{Name: "search_web", Description: "Search the web", Params: []tools.ParamSchema{{Name: "query", Type: "string"}}})

	result := buildTriageSystemPrompt(registry, "Thursday 6 March 2026, 10:00")
	if !strings.Contains(result, "search_web") {
		t.Error("expected triage prompt to contain tool name 'search_web'")
	}
	if !strings.Contains(result, "Thursday 6 March 2026") {
		t.Error("expected triage prompt to contain current time")
	}
}

func TestBuildJobContext(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		result := buildJobContext(nil)
		if result != "" {
			t.Errorf("expected empty string, got %q", result)
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		result := buildJobContext([]*engine.BackgroundJob{})
		if result != "" {
			t.Errorf("expected empty string for empty slice, got %q", result)
		}
	})
}
