package textutil

import "testing"

func TestStripThinkTags(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no think tags",
			input: "Hello world",
			want:  "Hello world",
		},
		{
			name:  "single think block",
			input: "<think>reasoning here</think>The answer is 42",
			want:  "The answer is 42",
		},
		{
			name:  "multiline think block",
			input: "<think>\nline1\nline2\n</think>\nResult",
			want:  "Result",
		},
		{
			name:  "nested think tags (non-greedy matches inner first)",
			input: "<think>outer <think>inner</think> still thinking</think>Done",
			want:  "still thinking</think>Done",
		},
		{
			name:  "consecutive think blocks",
			input: "<think>first</think> <think>second</think>Done",
			want:  "Done",
		},
		{
			name:  "multiple think blocks",
			input: "<think>first</think>middle<think>second</think>end",
			want:  "middleend",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "only think tags",
			input: "<think>all thinking</think>",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripThinkTags(tt.input)
			if got != tt.want {
				t.Errorf("StripThinkTags(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStripCodeFences(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no fences",
			input: `{"name":"foo"}`,
			want:  `{"name":"foo"}`,
		},
		{
			name:  "json fence",
			input: "```json\n{\"key\":\"value\"}\n```",
			want:  `{"key":"value"}`,
		},
		{
			name:  "plain fence",
			input: "```\nhello world\n```",
			want:  "hello world",
		},
		{
			name:  "fence with content containing backticks",
			input: "```json\n{\"code\":\"use ``` for fences\"}\n```",
			want:  "{\"code\":\"use ``` for fences\"}",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "no opening fence",
			input: "just text\n```",
			want:  "just text\n```",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripCodeFences(tt.input)
			if got != tt.want {
				t.Errorf("StripCodeFences(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "clean JSON",
			input: `{"name":"test","value":42}`,
			want:  `{"name":"test","value":42}`,
		},
		{
			name:  "JSON surrounded by prose",
			input: `Here is the result: {"score":0.8} hope that helps`,
			want:  `{"score":0.8}`,
		},
		{
			name:  "nested braces",
			input: `{"outer":{"inner":"value"}}`,
			want:  `{"outer":{"inner":"value"}}`,
		},
		{
			name:  "no JSON",
			input: "no json here",
			want:  "no json here",
		},
		{
			name:  "JSON with string containing braces",
			input: `{"text":"hello {world}"}`,
			want:  `{"text":"hello {world}"}`,
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "JSON with escaped quotes",
			input: `prefix {"key":"val\"ue"} suffix`,
			want:  `{"key":"val\"ue"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractJSON(tt.input)
			if got != tt.want {
				t.Errorf("ExtractJSON(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
