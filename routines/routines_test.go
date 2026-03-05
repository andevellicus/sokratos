package routines

import (
	"testing"
)

func TestIsEmptyResult(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty string", "", true},
		{"No tweets found", "No tweets found.", true},
		{"error connecting", "error connecting to API", true},
		{"Error uppercase", "Error: rate limited", true},
		{"no lowercase", "no results available", true},
		{"json count zero", `{"count":0, "items":[]}`, true},
		{"valid content", "Here are 3 new articles about AI...", false},
		{"short No alone", "No", false}, // len("No") == 2, not > len("No ")
		{"json with data", `{"count":5, "items":["a","b"]}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsEmptyResult(tt.input); got != tt.want {
				t.Errorf("IsEmptyResult(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNilIfEmpty(t *testing.T) {
	if got := NilIfEmpty(""); got != nil {
		t.Errorf("NilIfEmpty(\"\") = %v, want nil", got)
	}
	if got := NilIfEmpty("hello"); got == nil {
		t.Error("NilIfEmpty(\"hello\") = nil, want non-nil")
	} else if *got != "hello" {
		t.Errorf("NilIfEmpty(\"hello\") = %q, want \"hello\"", *got)
	}
}

func TestValidateSchedules(t *testing.T) {
	tests := []struct {
		name    string
		input   any
		wantErr bool
	}{
		{"nil", nil, false},
		{"valid single", "06:00", false},
		{"valid multi comma", "06:00,18:00", false},
		{"invalid entry", "25:00", true},
		{"string slice", []string{"06:00", "12:00"}, false},
		{"string slice invalid", []string{"06:00", "99:00"}, true},
		{"interface slice", []any{"08:00", "20:00"}, false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSchedules(tt.input)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateSchedules(%v) = nil, want error", tt.input)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateSchedules(%v) = %v, want nil", tt.input, err)
			}
		})
	}
}
