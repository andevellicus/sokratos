package objectives

import (
	"strings"
	"testing"
	"time"
)

func TestFormatList(t *testing.T) {
	tests := []struct {
		name string
		objs []Objective
		want []string // substrings that must appear
	}{
		{
			name: "empty slice",
			objs: nil,
			want: []string{"No objectives found."},
		},
		{
			name: "single objective",
			objs: []Objective{
				{ID: 1, Summary: "Learn Go", Status: "active", Priority: "high", Source: "explicit"},
			},
			want: []string{"#1", "[active]", "(high, explicit)", "Learn Go"},
		},
		{
			name: "with attempts",
			objs: []Objective{
				{ID: 2, Summary: "Build API", Status: "in_progress", Priority: "medium", Source: "inferred", Attempts: 3},
			},
			want: []string{"attempts: 3"},
		},
		{
			name: "with last pursued",
			objs: []Objective{
				{ID: 3, Summary: "Deploy", Status: "active", Priority: "low", Source: "explicit",
					LastPursued: timePtr(time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC))},
			},
			want: []string{"last pursued:"},
		},
		{
			name: "with progress notes",
			objs: []Objective{
				{ID: 4, Summary: "Research", Status: "active", Priority: "high", Source: "explicit",
					ProgressNotes: "Step 1 done"},
			},
			want: []string{"Progress:", "Step 1 done"},
		},
		{
			name: "long progress notes truncated",
			objs: []Objective{
				{ID: 5, Summary: "Big task", Status: "active", Priority: "high", Source: "explicit",
					ProgressNotes: string(make([]byte, 300))},
			},
			want: []string{"..."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatList(tt.objs)
			for _, sub := range tt.want {
				if !strings.Contains(got, sub) {
					t.Errorf("FormatList() missing %q in:\n%s", sub, got)
				}
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		n     int
		want  string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"needs truncation", "hello world", 5, "hello..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncate(tt.input, tt.n); got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }
