package orchestrate

import (
	"testing"
)

func TestIsToolSoftError(t *testing.T) {
	tests := []struct {
		name   string
		result string
		want   bool
	}{
		{"empty", "", false},
		{"json object", `{"data": "value"}`, false},
		{"json array", `[1, 2, 3]`, false},
		{"error message", "error: connection refused", true},
		{"soft error", "Failed to fetch emails: connection timeout", true},
		{"normal result", "Found 3 events for today", false},
		{"count-prefixed", "3 results found for today's meeting", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsToolSoftError(tc.result)
			if got != tc.want {
				t.Errorf("IsToolSoftError(%q) = %v, want %v", tc.result, got, tc.want)
			}
		})
	}
}
