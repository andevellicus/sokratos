package engine

import (
	"strings"
	"testing"
	"time"

	objpkg "sokratos/objectives"
)

func TestHeartbeatContext_Empty(t *testing.T) {
	hc := heartbeatContext{
		currentTime: "2026-03-04 14:30",
	}
	xml := hc.toXML()

	for _, want := range []string{
		"<work_items>none</work_items>",
		"<recent_actions>none</recent_actions>",
		"<active_objectives>none</active_objectives>",
		"<current_objective>none</current_objective>",
		"<user_last_active>never</user_last_active>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("toXML() empty context missing %q", want)
		}
	}
}

func TestHeartbeatContext_WorkItems(t *testing.T) {
	hc := heartbeatContext{
		currentTime: "2026-03-04 14:30",
		workItems: []workItem{
			{
				ID:        42,
				Type:      "scheduled",
				Directive: "Send daily report",
				Status:    "pending",
				Priority:  5,
			},
		},
	}
	xml := hc.toXML()

	for _, want := range []string{
		`type="scheduled"`,
		`status="pending"`,
		"Send daily report",
		`id="42"`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("toXML() work items missing %q", want)
		}
	}
}

func TestHeartbeatContext_Objectives(t *testing.T) {
	hc := heartbeatContext{
		currentTime: "2026-03-04 14:30",
		objectives: []objpkg.Objective{
			{
				ID:       1,
				Summary:  "Learn Rust",
				Status:   "active",
				Priority: "high",
				Attempts: 2,
			},
		},
	}
	xml := hc.toXML()

	for _, want := range []string{
		"<objective",
		`id="1"`,
		`status="active"`,
		`priority="high"`,
		`attempts="2"`,
		"Learn Rust",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("toXML() objectives missing %q", want)
		}
	}
}

func TestHeartbeatContext_RecentActions(t *testing.T) {
	hc := heartbeatContext{
		currentTime: "2026-03-04 14:30",
		recentActions: []actionRecord{
			{
				Time:    time.Date(2026, 3, 4, 14, 0, 0, 0, time.UTC),
				Type:    "routine",
				Summary: "Ran morning briefing",
			},
		},
	}
	xml := hc.toXML()

	for _, want := range []string{
		`<action type="routine"`,
		"Ran morning briefing",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("toXML() recent actions missing %q", want)
		}
	}
}
