package engine

import (
	"sokratos/llm"
	"testing"
)

func TestTrimMessages(t *testing.T) {
	tests := []struct {
		name       string
		msgs       []llm.Message
		maxTail    int
		wantLen    int
		wantFirst  string // content of messages[1] after trim
	}{
		{
			name: "under limit no-op",
			msgs: buildMessages(
				llm.Message{Role: "user", Content: "hello"},
				llm.Message{Role: "assistant", Content: "hi"},
			),
			maxTail:   5,
			wantLen:   3,
			wantFirst: "hello",
		},
		{
			name: "simple trim keeps system and last N",
			msgs: buildMessages(
				llm.Message{Role: "user", Content: "q1"},
				llm.Message{Role: "assistant", Content: "a1"},
				llm.Message{Role: "user", Content: "q2"},
				llm.Message{Role: "assistant", Content: "a2"},
			),
			maxTail:   2,
			wantLen:   3, // system + q2 + a2
			wantFirst: "q2",
		},
		{
			name: "tool result at cutoff backs up",
			msgs: buildMessages(
				llm.Message{Role: "user", Content: "q1"},
				llm.Message{Role: "assistant", Content: `{"name":"foo","arguments":{}}`},
				llm.Message{Role: "user", Content: "Tool result: data"},
				llm.Message{Role: "user", Content: "q2"},
				llm.Message{Role: "assistant", Content: "a2"},
			),
			maxTail: 3,
			// naive cutoff=3 → "Tool result: data" (isToolMessage) → back up to 2
			// messages[2] is assistant tool call (not isToolMessage) → stop
			wantLen:   5, // system + [2:]
			wantFirst: `{"name":"foo","arguments":{}}`,
		},
		{
			name: "exact fit",
			msgs: buildMessages(
				llm.Message{Role: "user", Content: "q1"},
				llm.Message{Role: "assistant", Content: "a1"},
			),
			maxTail:   2,
			wantLen:   3,
			wantFirst: "q1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimMessages(tt.msgs, tt.maxTail)
			if len(got) != tt.wantLen {
				t.Errorf("TrimMessages() len = %d, want %d", len(got), tt.wantLen)
			}
			if got[0].Role != "system" {
				t.Errorf("TrimMessages()[0].Role = %q, want system", got[0].Role)
			}
			if len(got) > 1 && got[1].Content != tt.wantFirst {
				t.Errorf("TrimMessages()[1].Content = %q, want %q", got[1].Content, tt.wantFirst)
			}
		})
	}
}

func TestIsToolMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  llm.Message
		want bool
	}{
		{"tool role", llm.Message{Role: "tool", Content: "result"}, true},
		{"tool result prefix", llm.Message{Role: "user", Content: "Tool result: data"}, true},
		{"tool error prefix", llm.Message{Role: "user", Content: "Tool error: failed"}, true},
		{"normal user", llm.Message{Role: "user", Content: "hello"}, false},
		{"assistant", llm.Message{Role: "assistant", Content: "hi"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isToolMessage(tt.msg); got != tt.want {
				t.Errorf("isToolMessage(%+v) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestStripAgentState(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "has agent state block",
			input: "Hello world\n\n[Current Agent State]\nstatus: idle\ntask: none",
			want:  "Hello world",
		},
		{
			name:  "no block",
			input: "Hello world",
			want:  "Hello world",
		},
		{
			name:  "block at start",
			input: "\n\n[Current Agent State]\nstatus: idle",
			want:  "",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripAgentState(tt.input); got != tt.want {
				t.Errorf("stripAgentState() = %q, want %q", got, tt.want)
			}
		})
	}
}
