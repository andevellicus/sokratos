package platform

import "testing"

func TestPipelineID(t *testing.T) {
	tests := []struct {
		id   string
		want int64
	}{
		{"12345", 12345},
		{"0", 0},
		{"", 0},
		{"not-a-number", 0},
		{"-1", -1},
	}
	for _, tt := range tests {
		m := &IncomingMessage{ID: tt.id}
		if got := m.PipelineID(); got != tt.want {
			t.Errorf("PipelineID(%q) = %d, want %d", tt.id, got, tt.want)
		}
	}
}
