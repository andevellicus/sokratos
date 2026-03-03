package routines

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestExpandArgs_BasicTemplates(t *testing.T) {
	now := time.Now()
	todayStr := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Format(timeLayout)
	tomorrowStr := now.AddDate(0, 0, 1)
	tomorrowStart := time.Date(tomorrowStr.Year(), tomorrowStr.Month(), tomorrowStr.Day(), 0, 0, 0, 0, now.Location()).Format(timeLayout)
	yesterdayStr := now.AddDate(0, 0, -1).Format("2006-01-02") // just check prefix

	args := map[string]interface{}{
		"time_min": "{{today}}",
		"time_max": "{{tomorrow}}",
		"since":    "{{yesterday}}",
		"current":  "{{now}}",
	}

	result := ExpandArgs(args)

	if result["time_min"] != todayStr {
		t.Errorf("time_min = %q, want %q", result["time_min"], todayStr)
	}
	if result["time_max"] != tomorrowStart {
		t.Errorf("time_max = %q, want %q", result["time_max"], tomorrowStart)
	}
	sinceStr, ok := result["since"].(string)
	if !ok || !strings.HasPrefix(sinceStr, yesterdayStr) {
		t.Errorf("since = %q, want prefix %q", sinceStr, yesterdayStr)
	}
	currentStr, ok := result["current"].(string)
	if !ok || currentStr == "{{now}}" {
		t.Errorf("current was not expanded: %q", currentStr)
	}
}

func TestExpandArgs_RelativeOffsets(t *testing.T) {
	now := time.Now()

	args := map[string]interface{}{
		"a": "{{now-2h}}",
		"b": "{{today+3d}}",
		"c": "{{now+30m}}",
		"d": "{{yesterday-1w}}",
		"e": "{{tomorrow+0d}}",
	}

	result := ExpandArgs(args)

	// {{now-2h}} should be approximately 2 hours ago.
	a, _ := time.ParseInLocation(timeLayout, result["a"].(string), now.Location())
	diffA := now.Sub(a)
	if diffA < 119*time.Minute || diffA > 121*time.Minute {
		t.Errorf("{{now-2h}}: got %s, diff from now = %v (want ~2h)", result["a"], diffA)
	}

	// {{today+3d}} should be midnight 3 days from now.
	b, _ := time.ParseInLocation(timeLayout, result["b"].(string), now.Location())
	wantB := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 3)
	if !b.Equal(wantB) {
		t.Errorf("{{today+3d}} = %s, want %s", b, wantB)
	}

	// {{now+30m}} should be approximately 30 minutes from now.
	c, _ := time.ParseInLocation(timeLayout, result["c"].(string), now.Location())
	diffC := c.Sub(now)
	if diffC < 29*time.Minute || diffC > 31*time.Minute {
		t.Errorf("{{now+30m}}: got %s, diff from now = %v (want ~30m)", result["c"], diffC)
	}

	// {{yesterday-1w}} should be 8 days ago at midnight.
	d, _ := time.ParseInLocation(timeLayout, result["d"].(string), now.Location())
	wantD := now.AddDate(0, 0, -1) // yesterday
	wantD = time.Date(wantD.Year(), wantD.Month(), wantD.Day(), 0, 0, 0, 0, now.Location())
	wantD = wantD.AddDate(0, 0, -7) // minus 1 week
	if !d.Equal(wantD) {
		t.Errorf("{{yesterday-1w}} = %s, want %s", d, wantD)
	}

	// {{tomorrow+0d}} should be tomorrow midnight (no-op offset).
	e, _ := time.ParseInLocation(timeLayout, result["e"].(string), now.Location())
	wantE := now.AddDate(0, 0, 1)
	wantE = time.Date(wantE.Year(), wantE.Month(), wantE.Day(), 0, 0, 0, 0, now.Location())
	if !e.Equal(wantE) {
		t.Errorf("{{tomorrow+0d}} = %s, want %s", e, wantE)
	}
}

func TestExpandArgs_NonStringPassthrough(t *testing.T) {
	args := map[string]interface{}{
		"max_results": float64(10),
		"enabled":     true,
		"query":       "tech",
		"time_min":    "{{today}}",
	}
	result := ExpandArgs(args)

	if result["max_results"] != float64(10) {
		t.Errorf("max_results = %v, want 10", result["max_results"])
	}
	if result["enabled"] != true {
		t.Errorf("enabled = %v, want true", result["enabled"])
	}
	if result["query"] != "tech" {
		t.Errorf("query = %v, want 'tech'", result["query"])
	}
	if result["time_min"] == "{{today}}" {
		t.Error("time_min was not expanded")
	}
}

func TestExpandArgs_NoTemplates(t *testing.T) {
	args := map[string]interface{}{
		"query": "hello world",
		"count": float64(5),
	}
	result := ExpandArgs(args)
	if result["query"] != "hello world" {
		t.Errorf("query = %v, want 'hello world'", result["query"])
	}
}

func TestExpandArgs_Nil(t *testing.T) {
	if result := ExpandArgs(nil); result != nil {
		t.Errorf("ExpandArgs(nil) = %v, want nil", result)
	}
}

func TestExpandAndMarshal(t *testing.T) {
	raw := json.RawMessage(`{"time_min":"{{today}}","max_results":5}`)
	result := ExpandAndMarshal(raw)

	var m map[string]interface{}
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	timeMin, ok := m["time_min"].(string)
	if !ok || timeMin == "{{today}}" {
		t.Errorf("time_min was not expanded: %v", m["time_min"])
	}
	if !strings.Contains(timeMin, "T00:00:00") {
		t.Errorf("time_min = %q, want *T00:00:00", timeMin)
	}

	maxResults, ok := m["max_results"].(float64)
	if !ok || maxResults != 5 {
		t.Errorf("max_results = %v, want 5", m["max_results"])
	}
}

func TestExpandAndMarshal_Empty(t *testing.T) {
	empty := json.RawMessage("")
	if result := ExpandAndMarshal(empty); len(result) != 0 {
		t.Errorf("ExpandAndMarshal(empty) = %q, want empty", result)
	}
}

func TestExpandAndMarshal_WithOffset(t *testing.T) {
	raw := json.RawMessage(`{"since":"{{now-24h}}","query":"test"}`)
	result := ExpandAndMarshal(raw)

	var m map[string]interface{}
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	since, ok := m["since"].(string)
	if !ok || since == "{{now-24h}}" {
		t.Errorf("since was not expanded: %v", m["since"])
	}
	if m["query"] != "test" {
		t.Errorf("query = %v, want 'test'", m["query"])
	}
}
