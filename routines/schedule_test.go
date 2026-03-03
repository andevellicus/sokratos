package routines

import (
	"testing"
	"time"
)

func TestNormalizeSchedule(t *testing.T) {
	tests := []struct {
		name string
		in   interface{}
		want string
	}{
		{"string", "06:00", "06:00"},
		{"string array", []string{"06:00", "18:00"}, "06:00,18:00"},
		{"interface array", []interface{}{"06:00", "18:00"}, "06:00,18:00"},
		{"nil", nil, ""},
		{"int", 42, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeSchedule(tt.in)
			if got != tt.want {
				t.Errorf("NormalizeSchedule(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSchedules(t *testing.T) {
	tests := []struct {
		in   string
		want int // expected count
	}{
		{"", 0},
		{"06:00", 1},
		{"06:00,18:00", 2},
		{"06:00, 12:00, 18:00", 3},
	}
	for _, tt := range tests {
		got := ParseSchedules(tt.in)
		if len(got) != tt.want {
			t.Errorf("ParseSchedules(%q) returned %d items, want %d", tt.in, len(got), tt.want)
		}
	}
}

func TestIsScheduleDue_SingleTime(t *testing.T) {
	now := time.Now()
	h := now.Hour()
	m := now.Minute()

	// A schedule 1 minute in the past, last executed yesterday → due.
	pastMinute := m - 1
	pastHour := h
	if pastMinute < 0 {
		pastMinute = 59
		pastHour = h - 1
		if pastHour < 0 {
			t.Skip("cannot test near midnight")
		}
	}
	pastSched := time.Date(now.Year(), now.Month(), now.Day(), pastHour, pastMinute, 0, 0, now.Location())
	_ = pastSched
	schedStr := formatHHMM(pastHour, pastMinute)
	yesterday := now.AddDate(0, 0, -1)

	if !IsScheduleDue([]string{schedStr}, yesterday) {
		t.Errorf("IsScheduleDue(%q, yesterday) = false, want true", schedStr)
	}

	// Same schedule, last executed after the target today → not due.
	if IsScheduleDue([]string{schedStr}, now) {
		t.Errorf("IsScheduleDue(%q, now) = true, want false", schedStr)
	}
}

func TestIsScheduleDue_MultiTime(t *testing.T) {
	now := time.Now()
	h := now.Hour()
	m := now.Minute()

	// Need at least 2 minutes into the hour for this test.
	if h == 0 && m < 2 {
		t.Skip("cannot test near midnight")
	}

	pastMinute := m - 1
	pastHour := h
	if pastMinute < 0 {
		pastMinute = 59
		pastHour = h - 1
	}

	futureMinute := m + 1
	futureHour := h
	if futureMinute > 59 {
		futureMinute = 0
		futureHour = h + 1
	}
	if futureHour > 23 {
		t.Skip("cannot test near end of day")
	}

	past := formatHHMM(pastHour, pastMinute)
	future := formatHHMM(futureHour, futureMinute)
	yesterday := now.AddDate(0, 0, -1)

	// Multi-schedule with one past, one future → due (past one fires).
	if !IsScheduleDue([]string{past, future}, yesterday) {
		t.Errorf("IsScheduleDue([%s, %s], yesterday) = false, want true", past, future)
	}

	// Multi-schedule with only future times → not due.
	if IsScheduleDue([]string{future}, yesterday) {
		t.Errorf("IsScheduleDue([%s], yesterday) = true, want false", future)
	}
}

func TestIsScheduleDue_AlreadyFiredToday(t *testing.T) {
	now := time.Now()
	h := now.Hour()
	m := now.Minute()

	pastMinute := m - 1
	pastHour := h
	if pastMinute < 0 {
		pastMinute = 59
		pastHour = h - 1
		if pastHour < 0 {
			t.Skip("cannot test near midnight")
		}
	}

	sched := formatHHMM(pastHour, pastMinute)
	// Last executed 1 second after the target → already fired today.
	target := time.Date(now.Year(), now.Month(), now.Day(), pastHour, pastMinute, 1, 0, now.Location())

	if IsScheduleDue([]string{sched}, target) {
		t.Errorf("IsScheduleDue(%q, after_target) = true, want false", sched)
	}
}

func formatHHMM(h, m int) string {
	return time.Date(2000, 1, 1, h, m, 0, 0, time.UTC).Format("15:04")
}
