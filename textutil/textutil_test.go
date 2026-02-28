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
			name:  "nested think tags (strips all reasoning)",
			input: "<think>outer <think>inner</think> still thinking</think>Done",
			want:  "Done",
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
		{
			name:  "orphaned </think> from template-injected <think> (Z1 pattern)",
			input: "Okay let me think about this.\n</think>\n{\"answer\": 42}",
			want:  "{\"answer\": 42}",
		},
		{
			name:  "orphaned </think> with reasoning containing JSON",
			input: "The key {\"k\":\"v\"} is important.\n</think>\n{\"real\":\"output\"}",
			want:  "{\"real\":\"output\"}",
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
		{
			name:  "truncated JSON returns partial match",
			input: `Some prose {"key": "val`, // depth never returns to 0
			want:  `{"key": "val`,
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

func TestStripCodeFences_Embedded(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "prose then code fence",
			input: "Here is the profile:\n```json\n{\"key\":\"value\"}\n```",
			want:  `{"key":"value"}`,
		},
		{
			name:  "prose then code fence with trailing text",
			input: "Profile:\n```json\n{\"a\":1}\n```\nDone.",
			want:  `{"a":1}`,
		},
		{
			name:  "no fence at all",
			input: `{"key":"value"}`,
			want:  `{"key":"value"}`,
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

func TestCleanLLMJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "clean JSON passthrough",
			input: `{"key":"value"}`,
			want:  `{"key":"value"}`,
		},
		{
			name:  "think tags + code fences + prose",
			input: "<think>reasoning</think>Here:\n```json\n{\"a\":1}\n```\nDone.",
			want:  `{"a":1}`,
		},
		{
			name:  "trailing comma",
			input: `{"items":["a","b",]}`,
			want:  `{"items":["a","b"]}`,
		},
		{
			name:  "trailing dot in number",
			input: `{"score":7.}`,
			want:  `{"score":7}`,
		},
		{
			name:  "Z1 reasoning preamble with orphaned </think>",
			input: "Okay let me think. The key {\"k\":\"v\"} matters.\n</think>\n{\"personality\": [{\"a\":\"b\"}], \"user_profile\": {\"name\": \"Jan\"}}",
			want:  `{"personality": [{"a":"b"}], "user_profile": {"name": "Jan"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CleanLLMJSON(tt.input)
			if got != tt.want {
				t.Errorf("CleanLLMJSON(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
