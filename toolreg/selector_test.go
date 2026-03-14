package toolreg

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestBuildSelectedToolIndex_FiltersCorrectly(t *testing.T) {
	r := NewRegistry()
	noop := func(_ context.Context, _ json.RawMessage) (string, error) { return "", nil }
	r.Register("search_web", noop, ToolSchema{Name: "search_web", Description: "Search the web", Params: []ParamSchema{{Name: "query", Type: "string", Required: true}}})
	r.Register("send_email", noop, ToolSchema{Name: "send_email", Description: "Send an email"})

	idx := BuildSelectedToolIndex(r, []string{"search_web"})
	if idx == "" {
		t.Fatal("expected non-empty index")
	}
	if !strings.Contains(idx, "search_web") {
		t.Error("expected search_web in index")
	}
	if strings.Contains(idx, "send_email") {
		t.Error("did not expect send_email in index")
	}
}

func TestBuildSelectedToolIndex_SkillsSeparate(t *testing.T) {
	r := NewRegistry()
	noop := func(_ context.Context, _ json.RawMessage) (string, error) { return "", nil }
	r.Register("search_web", noop, ToolSchema{Name: "search_web", Description: "Search the web"})
	r.Register("my_skill", noop, ToolSchema{Name: "my_skill", Description: "A custom skill", IsSkill: true})

	idx := BuildSelectedToolIndex(r, []string{"search_web", "my_skill"})
	if !strings.Contains(idx, "## Skills") {
		t.Error("expected Skills section for skill tool")
	}
	if !strings.Contains(idx, "my_skill") {
		t.Error("expected my_skill in index")
	}
}

func TestAffinityGroupExpansion(t *testing.T) {
	selected := map[string]struct{}{
		"search_email": {},
	}

	for _, group := range affinityGroups {
		hit := false
		for _, toolName := range group {
			if _, ok := selected[toolName]; ok {
				hit = true
				break
			}
		}
		if hit {
			for _, toolName := range group {
				selected[toolName] = struct{}{}
			}
		}
	}

	if _, ok := selected["send_email"]; !ok {
		t.Error("expected send_email to be added via affinity group")
	}
}

func TestCoreToolsAlwaysIncluded(t *testing.T) {
	selected := make(map[string]struct{})
	for t := range coreTools {
		selected[t] = struct{}{}
	}

	names := make([]string, 0, len(selected))
	for t := range selected {
		names = append(names, t)
	}
	sort.Strings(names)

	expectedCore := []string{"deep_think", "save_memory", "search_memory", "search_web"}
	for _, expected := range expectedCore {
		found := false
		for _, n := range names {
			if n == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in core tools", expected)
		}
	}
}
