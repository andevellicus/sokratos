package pipelines

import (
	"testing"
)

func TestTruncateAssistantReply(t *testing.T) {
	tests := []struct {
		name        string
		exchange    string
		maxReplyLen int
		want        string
	}{
		{
			name:        "no assistant prefix unchanged",
			exchange:    "user: hello there",
			maxReplyLen: 100,
			want:        "user: hello there",
		},
		{
			name:        "reply already short",
			exchange:    "user: hi\nassistant: hello",
			maxReplyLen: 100,
			want:        "user: hi\nassistant: hello",
		},
		{
			name:        "reply too long gets truncated",
			exchange:    "user: hi\nassistant: " + longString(200),
			maxReplyLen: 50,
			want:        "user: hi\nassistant: " + longString(50) + "...",
		},
		{
			name:        "user part preserved on truncation",
			exchange:    "user: important question about life\nassistant: " + longString(200),
			maxReplyLen: 30,
			want:        "user: important question about life\nassistant: " + longString(30) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateAssistantReply(tt.exchange, tt.maxReplyLen)
			if got != tt.want {
				t.Errorf("truncateAssistantReply() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// longString creates a string of n repeated 'x' characters.
func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func TestHasPreferenceTags(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want bool
	}{
		{"match preferences", []string{"general", "preferences"}, true},
		{"match behavior", []string{"behavior"}, true},
		{"match communication_style", []string{"communication_style"}, true},
		{"match response_style", []string{"response_style"}, true},
		{"no match general", []string{"general", "email"}, false},
		{"empty tags", []string{}, false},
		{"nil tags", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasPreferenceTags(tt.tags)
			if got != tt.want {
				t.Errorf("hasPreferenceTags(%v) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}
