package memory

import (
	"testing"
)

func TestExtractSummary(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "has source section",
			input: "User discussed AI safety concerns\n\nSource conversation:\nUser: What about AI?\nBot: Good question.",
			want:  "User discussed AI safety concerns",
		},
		{
			name:  "no source",
			input: "A distilled fact about the user's preferences.",
			want:  "A distilled fact about the user's preferences.",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
		{
			name:  "source at start",
			input: "\n\nSource email:\nFrom: test@example.com",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractSummary(tt.input); got != tt.want {
				t.Errorf("ExtractSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}
