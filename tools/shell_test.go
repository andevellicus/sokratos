package tools

import (
	"testing"
)

func TestExtractBinaries(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "single command",
			command: "ls -la /tmp",
			want:    []string{"ls"},
		},
		{
			name:    "two commands with &&",
			command: "free -h && df -h /",
			want:    []string{"free", "df"},
		},
		{
			name:    "pipe",
			command: "ps aux | head -5",
			want:    []string{"ps", "head"},
		},
		{
			name:    "complex compound",
			command: "echo '=== Memory ===' && free -h && echo '=== Disk ===' && df -h / /home && uptime",
			want:    []string{"echo", "free", "df", "uptime"},
		},
		{
			name:    "semicolons",
			command: "uptime; free -h; df -h",
			want:    []string{"uptime", "free", "df"},
		},
		{
			name:    "or operator",
			command: "curl http://example.com || wget http://example.com",
			want:    []string{"curl", "wget"},
		},
		{
			name:    "deduplication",
			command: "echo hello && echo world",
			want:    []string{"echo"},
		},
		{
			name:    "pipe and &&",
			command: "ps aux --sort=-%cpu | head -5 && free -h",
			want:    []string{"ps", "head", "free"},
		},
		{
			name:    "empty",
			command: "",
			want:    nil,
		},
		{
			name:    "whitespace only",
			command: "   ",
			want:    nil,
		},
		{
			name:    "path traversal stripped",
			command: "/usr/bin/ls -la",
			want:    []string{"ls"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractBinaries(tt.command)
			if len(got) != len(tt.want) {
				t.Fatalf("extractBinaries(%q) = %v, want %v", tt.command, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractBinaries(%q)[%d] = %q, want %q", tt.command, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSplitShellCompound(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "no operators",
			command: "ls -la",
			want:    []string{"ls -la"},
		},
		{
			name:    "&&",
			command: "free -h && df -h",
			want:    []string{"free -h", "df -h"},
		},
		{
			name:    "|| before |",
			command: "curl url || wget url | head",
			want:    []string{"curl url", "wget url", "head"},
		},
		{
			name:    "mixed operators",
			command: "a && b; c | d || e",
			want:    []string{"a", "b", "c", "d", "e"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitShellCompound(tt.command)
			if len(got) != len(tt.want) {
				t.Fatalf("splitShellCompound(%q) = %v, want %v", tt.command, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitShellCompound(%q)[%d] = %q, want %q", tt.command, i, got[i], tt.want[i])
				}
			}
		})
	}
}
