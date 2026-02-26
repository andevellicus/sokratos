package engine

import (
	"context"
	"os"
	"testing"

	"sokratos/llm"
	"sokratos/logger"
)

func TestMain(m *testing.M) {
	if err := logger.Init(os.TempDir()); err != nil {
		panic(err)
	}
	defer logger.Close()
	os.Exit(m.Run())
}

// buildMessages creates a message slice with a system prompt at index 0
// followed by the given messages.
func buildMessages(msgs ...llm.Message) []llm.Message {
	all := make([]llm.Message, 0, 1+len(msgs))
	all = append(all, llm.Message{Role: "system", Content: "You are a helpful assistant."})
	all = append(all, msgs...)
	return all
}

func TestSafeCutoffNeverSplitsToolCallPair(t *testing.T) {
	//   [0] system
	//   [1] user: "hello"
	//   [2] assistant: tool call
	//   [3] user: "Tool result: ..."
	//   [4] user: "what is 2+2?"
	//   [5] assistant: "4"
	msgs := buildMessages(
		llm.Message{Role: "user", Content: "hello"},
		llm.Message{Role: "assistant", Content: `{"name":"get_server_time","arguments":{}}`},
		llm.Message{Role: "user", Content: "Tool result: 2024-01-01T00:00:00Z"},
		llm.Message{Role: "user", Content: "what is 2+2?"},
		llm.Message{Role: "assistant", Content: "4"},
	)

	sm := &StateManager{messages: msgs}

	// maxMessages=3 → naive cutoff = 6-3 = 3.
	// Backward from 3: all tool messages → no safe boundary.
	// Forward from 4: i=4 "what is 2+2?" → safe! safeIndex=4.
	// Keeps [0] + [4,5] = 3 messages. Tool pair (2,3) removed together, not split.
	SlideAndArchiveContext(context.Background(), sm, 3, nil, "", "")

	if len(sm.messages) != 3 {
		t.Errorf("expected 3 messages after slide, got %d", len(sm.messages))
	}
	if sm.messages[1].Content != "what is 2+2?" {
		t.Errorf("expected 'what is 2+2?' at [1], got %q", sm.messages[1].Content)
	}
}

func TestCutsAtUserBoundary(t *testing.T) {
	//   [0] system
	//   [1] user: "first question"
	//   [2] assistant: "first answer"
	//   [3] user: "second question"
	//   [4] assistant: tool call
	//   [5] user: "Tool result: ..."
	//   [6] user: "third question"
	//   [7] assistant: "third answer"
	msgs := buildMessages(
		llm.Message{Role: "user", Content: "first question"},
		llm.Message{Role: "assistant", Content: "first answer"},
		llm.Message{Role: "user", Content: "second question"},
		llm.Message{Role: "assistant", Content: `{"name":"search_web","arguments":{"query":"test"}}`},
		llm.Message{Role: "user", Content: "Tool result: some results"},
		llm.Message{Role: "user", Content: "third question"},
		llm.Message{Role: "assistant", Content: "third answer"},
	)

	sm := &StateManager{messages: msgs}

	// maxMessages=4 → naive cutoff = 8-4 = 4.
	// Walk backward from 4:
	//   i=4: assistant tool call → isToolCallContent → not safe.
	//   i=3: user "second question" → not tool msg → safe! safeIndex=3.
	// Archive msgs[1:3] = [first_q, first_a]. Keep system + msgs[3:] = 6.
	SlideAndArchiveContext(context.Background(), sm, 4, nil, "", "")

	if len(sm.messages) != 6 {
		t.Errorf("expected 6 messages after slide, got %d", len(sm.messages))
	}
	if sm.messages[1].Content != "second question" {
		t.Errorf("expected 'second question' at [1], got %q", sm.messages[1].Content)
	}
}

func TestCutsAtAssistantBoundary(t *testing.T) {
	//   [0] system
	//   [1] user: "q1"
	//   [2] assistant: "a1"
	//   [3] assistant: tool call
	//   [4] user: "Tool result: data"
	//   [5] user: "q2"
	//   [6] assistant: "a2"
	msgs := buildMessages(
		llm.Message{Role: "user", Content: "q1"},
		llm.Message{Role: "assistant", Content: "a1"},
		llm.Message{Role: "assistant", Content: `{"name":"foo","arguments":{}}`},
		llm.Message{Role: "user", Content: "Tool result: data"},
		llm.Message{Role: "user", Content: "q2"},
		llm.Message{Role: "assistant", Content: "a2"},
	)

	sm := &StateManager{messages: msgs}

	// maxMessages=3 → naive cutoff = 7-3 = 4.
	// Walk backward from 4:
	//   i=4: user "Tool result:" → isToolMessage → not safe.
	//   i=3: assistant tool call → isToolCallContent → not safe.
	//   i=2: assistant "a1" → not tool call → safe! safeIndex=2.
	// Archive msgs[1:2] = [q1]. Keep system + msgs[2:] = 6.
	SlideAndArchiveContext(context.Background(), sm, 3, nil, "", "")

	if len(sm.messages) != 6 {
		t.Errorf("expected 6 messages after slide, got %d", len(sm.messages))
	}
	if sm.messages[1].Content != "a1" {
		t.Errorf("expected 'a1' at [1], got %q", sm.messages[1].Content)
	}
}

func TestAllToolMessages_Aborts(t *testing.T) {
	// Nothing but tool-call/result pairs after system — no safe boundary.
	//   [0] system
	//   [1] assistant: tool call
	//   [2] user: "Tool result: a"
	//   [3] assistant: tool call
	//   [4] user: "Tool result: b"
	//   [5] user: "finally a real question"
	//   [6] assistant: "answer"
	msgs := buildMessages(
		llm.Message{Role: "assistant", Content: `{"name":"t1","arguments":{}}`},
		llm.Message{Role: "user", Content: "Tool result: a"},
		llm.Message{Role: "assistant", Content: `{"name":"t2","arguments":{}}`},
		llm.Message{Role: "user", Content: "Tool result: b"},
		llm.Message{Role: "user", Content: "finally a real question"},
		llm.Message{Role: "assistant", Content: "answer"},
	)

	sm := &StateManager{messages: msgs}

	// maxMessages=3 → naive cutoff = 7-3 = 4.
	// Backward from 4: all tool messages → no safe boundary.
	// Forward from 5: i=5 "finally a real question" → safe! safeIndex=5.
	// Keeps [0] + [5,6] = 3 messages. Both tool pairs removed together.
	SlideAndArchiveContext(context.Background(), sm, 3, nil, "", "")

	if len(sm.messages) != 3 {
		t.Errorf("expected 3 messages after slide, got %d", len(sm.messages))
	}
	if sm.messages[1].Content != "finally a real question" {
		t.Errorf("expected 'finally a real question' at [1], got %q", sm.messages[1].Content)
	}
}

func TestFingerprintMismatchAbortsMutation(t *testing.T) {
	//   [0] system
	//   [1] user: "hello"
	//   [2] assistant: "hi there"
	//   [3] user: "how are you?"
	//   [4] assistant: "I'm good"
	//   [5] user: "great"
	//   [6] assistant: "anything else?"
	msgs := buildMessages(
		llm.Message{Role: "user", Content: "hello"},
		llm.Message{Role: "assistant", Content: "hi there"},
		llm.Message{Role: "user", Content: "how are you?"},
		llm.Message{Role: "assistant", Content: "I'm good"},
		llm.Message{Role: "user", Content: "great"},
		llm.Message{Role: "assistant", Content: "anything else?"},
	)

	sm := &StateManager{messages: msgs}
	original := sm.ReadMessages()

	// Mutate messages[1] to break the fingerprint.
	sm.mu.Lock()
	sm.messages[1].Content = "modified after snapshot"
	sm.mu.Unlock()

	snap := fingerprintMessages(original[1:4])
	current := fingerprintMessages(sm.ReadMessages()[1:4])

	if snap == current {
		t.Error("expected fingerprint mismatch after mutation, but they matched")
	}
}

func TestIsSafeBoundary(t *testing.T) {
	tests := []struct {
		name string
		msg  llm.Message
		want bool
	}{
		{"user message", llm.Message{Role: "user", Content: "hello"}, true},
		{"tool result", llm.Message{Role: "user", Content: "Tool result: data"}, false},
		{"tool error", llm.Message{Role: "user", Content: "Tool error: oops"}, false},
		{"assistant text", llm.Message{Role: "assistant", Content: "Sure, I can help."}, true},
		{"assistant tool call", llm.Message{Role: "assistant", Content: `{"name":"foo","arguments":{}}`}, false},
		{"tool role", llm.Message{Role: "tool", Content: "result"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSafeBoundary(tt.msg)
			if got != tt.want {
				t.Errorf("isSafeBoundary(%+v) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestIsToolCallContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"plain tool call", `{"name":"get_server_time","arguments":{}}`, true},
		{"fenced tool call", "```json\n{\"name\":\"search_web\",\"arguments\":{\"query\":\"test\"}}\n```", true},
		{"plain text", "Hello, how are you?", false},
		{"json without name", `{"foo":"bar"}`, false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isToolCallContent(tt.content)
			if got != tt.want {
				t.Errorf("isToolCallContent(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestFingerprintMessages(t *testing.T) {
	a := []llm.Message{{Role: "user", Content: "hello"}}
	b := []llm.Message{{Role: "user", Content: "hello"}}
	c := []llm.Message{{Role: "user", Content: "world"}}

	fpA := fingerprintMessages(a)
	fpB := fingerprintMessages(b)
	fpC := fingerprintMessages(c)

	if fpA != fpB {
		t.Error("identical messages should produce identical fingerprints")
	}
	if fpA == fpC {
		t.Error("different messages should produce different fingerprints")
	}
}
