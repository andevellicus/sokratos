package timefmt

import (
	"testing"
	"time"
)

func TestParseISO8601(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(time.Time) bool
		desc    string // describes the check
	}{
		{
			name:  "RFC3339 with timezone",
			input: "2026-03-04T14:30:00+05:00",
			check: func(t time.Time) bool {
				_, offset := t.Zone()
				return t.Hour() == 14 && t.Minute() == 30 && offset == 5*3600
			},
			desc: "hour=14, minute=30, offset=+5h",
		},
		{
			name:  "ISO8601 no timezone interpreted as local",
			input: "2026-03-04T14:30:00",
			check: func(parsed time.Time) bool {
				return parsed.Location() == time.Local && parsed.Hour() == 14
			},
			desc: "location=Local, hour=14",
		},
		{
			name:  "minute precision",
			input: "2026-03-04T14:30",
			check: func(parsed time.Time) bool {
				return parsed.Hour() == 14 && parsed.Minute() == 30 && parsed.Second() == 0
			},
			desc: "hour=14, minute=30, second=0",
		},
		{
			name:  "date only",
			input: "2026-03-04",
			check: func(parsed time.Time) bool {
				return parsed.Year() == 2026 && parsed.Month() == time.March && parsed.Day() == 4
			},
			desc: "2026-03-04",
		},
		{
			name:  "Z suffix interpreted as UTC",
			input: "2026-03-04T14:30:00Z",
			check: func(parsed time.Time) bool {
				return parsed.Location() == time.UTC && parsed.Hour() == 14
			},
			desc: "location=UTC, hour=14",
		},
		{
			name:    "invalid format",
			input:   "March 4th 2026",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseISO8601(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseISO8601(%q) = %v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseISO8601(%q) error = %v", tt.input, err)
			}
			if !tt.check(got) {
				t.Errorf("ParseISO8601(%q) = %v, want %s", tt.input, got, tt.desc)
			}
		})
	}
}

func TestParseSchedule(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantH      int
		wantM      int
		wantErr    bool
	}{
		{"valid 06:00", "06:00", 6, 0, false},
		{"valid 00:00", "00:00", 0, 0, false},
		{"valid 23:59", "23:59", 23, 59, false},
		{"invalid hour 24:00", "24:00", 0, 0, true},
		{"invalid minute 12:60", "12:60", 0, 0, true},
		{"wrong format single digit", "6:00", 0, 0, true},
		{"wrong format no colon", "0600", 0, 0, true},
		{"empty string", "", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, m, err := ParseSchedule(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseSchedule(%q) = (%d, %d, nil), want error", tt.input, h, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSchedule(%q) error = %v", tt.input, err)
			}
			if h != tt.wantH || m != tt.wantM {
				t.Errorf("ParseSchedule(%q) = (%d, %d), want (%d, %d)", tt.input, h, m, tt.wantH, tt.wantM)
			}
		})
	}
}

func TestReinterpretAsLocal(t *testing.T) {
	// UTC input should be reinterpreted with same wall clock in Local.
	utcTime := time.Date(2026, 3, 4, 14, 30, 0, 0, time.UTC)
	got := ReinterpretAsLocal(utcTime)
	if got.Location() != time.Local {
		t.Errorf("ReinterpretAsLocal(UTC) location = %v, want Local", got.Location())
	}
	if got.Hour() != 14 || got.Minute() != 30 {
		t.Errorf("ReinterpretAsLocal(UTC) = %v, want 14:30 wall clock", got)
	}

	// Non-UTC input should be returned unchanged.
	loc := time.FixedZone("EST", -5*3600)
	estTime := time.Date(2026, 3, 4, 14, 30, 0, 0, loc)
	got2 := ReinterpretAsLocal(estTime)
	if got2 != estTime {
		t.Errorf("ReinterpretAsLocal(EST) = %v, want unchanged %v", got2, estTime)
	}
}

func TestFormatDateTime(t *testing.T) {
	ts := time.Date(2026, 3, 4, 14, 30, 0, 0, time.UTC)
	want := "2026-03-04 14:30"
	if got := FormatDateTime(ts); got != want {
		t.Errorf("FormatDateTime() = %q, want %q", got, want)
	}
}

func TestFormatDate(t *testing.T) {
	ts := time.Date(2026, 3, 4, 14, 30, 0, 0, time.UTC)
	want := "2026-03-04"
	if got := FormatDate(ts); got != want {
		t.Errorf("FormatDate() = %q, want %q", got, want)
	}
}

func TestFormatNatural(t *testing.T) {
	ts := time.Date(2026, 3, 4, 14, 30, 0, 0, time.UTC)
	want := "Wednesday, March 4, 2026 at 2:30 PM"
	if got := FormatNatural(ts); got != want {
		t.Errorf("FormatNatural() = %q, want %q", got, want)
	}
}
