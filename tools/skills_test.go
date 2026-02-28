package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sokratos/logger"
)

func TestMain(m *testing.M) {
	// Initialize logger for tests that use LoadSkills/RegisterSkill.
	_ = logger.Init(os.TempDir())
	os.Exit(m.Run())
}

func TestParseSkillMD(t *testing.T) {
	md := `---
name: test_skill
description: A test skill for unit testing.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| query | string | yes |
| limit | number | no |

## Notes

Some extra notes.
`
	manifest, params, err := parseSkillMD([]byte(md))
	if err != nil {
		t.Fatalf("parseSkillMD error: %v", err)
	}
	if manifest.Name != "test_skill" {
		t.Errorf("expected name=test_skill, got %q", manifest.Name)
	}
	if manifest.Description != "A test skill for unit testing." {
		t.Errorf("expected description, got %q", manifest.Description)
	}
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(params))
	}
	if params[0].Name != "query" || params[0].Type != "string" || !params[0].Required {
		t.Errorf("unexpected param[0]: %+v", params[0])
	}
	if params[1].Name != "limit" || params[1].Type != "number" || params[1].Required {
		t.Errorf("unexpected param[1]: %+v", params[1])
	}
}

func TestParseSkillMD_MultilineDescription(t *testing.T) {
	md := `---
name: multi_desc
description: |
  Line one of the description.
  Line two of the description.
---
`
	manifest, _, err := parseSkillMD([]byte(md))
	if err != nil {
		t.Fatalf("parseSkillMD error: %v", err)
	}
	if manifest.Name != "multi_desc" {
		t.Errorf("expected name=multi_desc, got %q", manifest.Name)
	}
	if manifest.Description != "Line one of the description. Line two of the description." {
		t.Errorf("unexpected description: %q", manifest.Description)
	}
}

func TestParseSkillMD_NoParams(t *testing.T) {
	md := `---
name: no_params
description: A skill with no parameters.
---
`
	manifest, params, err := parseSkillMD([]byte(md))
	if err != nil {
		t.Fatalf("parseSkillMD error: %v", err)
	}
	if manifest.Name != "no_params" {
		t.Errorf("expected name=no_params, got %q", manifest.Name)
	}
	if len(params) != 0 {
		t.Errorf("expected 0 params, got %d", len(params))
	}
}

func TestParseSkillMD_MissingName(t *testing.T) {
	md := `---
description: No name field here.
---
`
	_, _, err := parseSkillMD([]byte(md))
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestValidateSkillSource(t *testing.T) {
	// Valid JS.
	if err := ValidateSkillSource(`var x = 1 + 2; x;`); err != nil {
		t.Errorf("expected valid JS, got error: %v", err)
	}

	// Invalid JS.
	if err := ValidateSkillSource(`var x = {;`); err == nil {
		t.Error("expected error for invalid JS")
	}

	// Top-level return (should pass via IIFE wrapping).
	if err := ValidateSkillSource(`var x = 1 + 2; return x;`); err != nil {
		t.Errorf("expected return-based JS to pass via IIFE, got error: %v", err)
	}

	// Top-level return with function body.
	src := `var resp = http_request("GET", "https://example.com"); return resp.body;`
	if err := ValidateSkillSource(src); err != nil {
		t.Errorf("expected return-in-function-body to pass via IIFE, got error: %v", err)
	}
}

func TestExecuteSkill_ReturnStatement(t *testing.T) {
	source := `var result = args.a * args.b; return "product=" + result;`
	args := json.RawMessage(`{"a": 5, "b": 6}`)
	result, err := ExecuteSkill(context.Background(), "test_return", source, args)
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "product=30" {
		t.Errorf("expected product=30, got %q", result)
	}
}

func TestNeedsIIFEWrap(t *testing.T) {
	if needsIIFEWrap(`var x = 1; x;`) {
		t.Error("expression-value source should not need IIFE wrap")
	}
	if !needsIIFEWrap(`return 42;`) {
		t.Error("top-level return should need IIFE wrap")
	}
}

func TestExecuteSkill_BasicArithmetic(t *testing.T) {
	source := `var result = args.a + args.b; "sum=" + result;`
	args := json.RawMessage(`{"a": 3, "b": 4}`)
	result, err := ExecuteSkill(context.Background(), "test", source, args)
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "sum=7" {
		t.Errorf("expected sum=7, got %q", result)
	}
}

func TestExecuteSkill_DefaultArgs(t *testing.T) {
	source := `var c = args.currency || "usd"; c;`
	args := json.RawMessage(`{}`)
	result, err := ExecuteSkill(context.Background(), "test", source, args)
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "usd" {
		t.Errorf("expected usd, got %q", result)
	}
}

func TestExecuteSkill_RuntimeError(t *testing.T) {
	source := `null.foo;`
	args := json.RawMessage(`{}`)
	result, err := ExecuteSkill(context.Background(), "test", source, args)
	if err != nil {
		t.Fatalf("expected soft error, got hard error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty soft error message")
	}
}

func TestValidateURL_BlocksPrivate(t *testing.T) {
	cases := []struct {
		url     string
		blocked bool
	}{
		{"http://127.0.0.1/test", true},
		{"http://192.168.1.1/test", true},
		{"http://10.0.0.1/test", true},
		{"ftp://example.com", true}, // wrong scheme
	}
	for _, tc := range cases {
		err := validateURL(tc.url)
		if tc.blocked && err == nil {
			t.Errorf("expected %s to be blocked", tc.url)
		}
	}
}

func TestValidateURL_AllowsPublic(t *testing.T) {
	err := validateURL("https://api.github.com")
	if err != nil {
		t.Errorf("expected public URL to be allowed, got: %v", err)
	}
}

func TestLoadSkills_NonexistentDir(t *testing.T) {
	skills, err := LoadSkills("/tmp/nonexistent-skills-dir-12345")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent dir, got: %v", err)
	}
	if skills != nil {
		t.Errorf("expected nil skills, got %d", len(skills))
	}
}

func TestLoadSkills_ValidSkill(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test_skill")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	md := `---
name: test_skill
description: A test skill.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| input | string | yes |
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)
	os.WriteFile(filepath.Join(scriptsDir, "handler.js"), []byte(`"hello " + args.input;`), 0644)

	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("LoadSkills error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Manifest.Name != "test_skill" {
		t.Errorf("expected test_skill, got %q", skills[0].Manifest.Name)
	}
	if len(skills[0].Params) != 1 {
		t.Errorf("expected 1 param, got %d", len(skills[0].Params))
	}
}

func TestRegisterSkill_AndExecute(t *testing.T) {
	registry := NewRegistry()
	skill := Skill{
		Manifest: SkillManifest{Name: "add_nums", Description: "Adds two numbers"},
		Params: []ParamSchema{
			{Name: "a", Type: "number", Required: true},
			{Name: "b", Type: "number", Required: true},
		},
		Source: `"result=" + (args.a + args.b);`,
	}
	RegisterSkill(registry, skill)

	if !registry.Has("add_nums") {
		t.Fatal("expected add_nums to be registered")
	}

	raw := json.RawMessage(`{"name":"add_nums","arguments":{"a":10,"b":20}}`)
	result, err := registry.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result != "result=30" {
		t.Errorf("expected result=30, got %q", result)
	}
}

func TestGenerateSkillMD(t *testing.T) {
	md := GenerateSkillMD("my_skill", "Does something useful.", []ParamSchema{
		{Name: "query", Type: "string", Required: true},
	})
	if md == "" {
		t.Fatal("expected non-empty SKILL.md content")
	}

	// Verify it round-trips through the parser.
	manifest, params, err := parseSkillMD([]byte(md))
	if err != nil {
		t.Fatalf("round-trip parse error: %v", err)
	}
	if manifest.Name != "my_skill" {
		t.Errorf("expected my_skill, got %q", manifest.Name)
	}
	if manifest.Description != "Does something useful." {
		t.Errorf("expected description, got %q", manifest.Description)
	}
	if len(params) != 1 || params[0].Name != "query" {
		t.Errorf("unexpected params: %+v", params)
	}
}

func TestLoadSkills_BuiltinSkills(t *testing.T) {
	skills, err := LoadSkills("../skills")
	if err != nil {
		t.Fatalf("LoadSkills error: %v", err)
	}

	expected := map[string]struct{}{
		"get_server_time": {},
	}

	loaded := make(map[string]struct{})
	for _, s := range skills {
		loaded[s.Manifest.Name] = struct{}{}

		if s.Manifest.Description == "" {
			t.Errorf("skill %q has empty description", s.Manifest.Name)
		}
		if s.Source == "" {
			t.Errorf("skill %q has empty source", s.Manifest.Name)
		}
	}

	for name := range expected {
		if _, ok := loaded[name]; !ok {
			t.Errorf("expected skill %q to be loaded", name)
		}
	}
}

func TestLoadSkills_BuiltinSkillParams(t *testing.T) {
	skills, err := LoadSkills("../skills")
	if err != nil {
		t.Fatalf("LoadSkills error: %v", err)
	}

	paramCounts := map[string]int{
		"get_server_time": 0,
	}

	for _, s := range skills {
		want, ok := paramCounts[s.Manifest.Name]
		if !ok {
			continue
		}
		if len(s.Params) != want {
			t.Errorf("skill %q: expected %d params, got %d", s.Manifest.Name, want, len(s.Params))
		}
	}
}

func TestGenerateSkillMD_RoundTripViaLoadSkills(t *testing.T) {
	// Simulate create_skill: generate SKILL.md + handler.js, then load via LoadSkills.
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test_generated")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	md := GenerateSkillMD("test_generated", "A tool-created skill for testing.", []ParamSchema{
		{Name: "city", Type: "string", Required: true},
		{Name: "units", Type: "string", Required: false},
	})
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)
	os.WriteFile(filepath.Join(scriptsDir, "handler.js"), []byte(`"weather in " + args.city;`), 0644)

	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("LoadSkills error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	if s.Manifest.Name != "test_generated" {
		t.Errorf("expected name=test_generated, got %q", s.Manifest.Name)
	}
	if s.Manifest.Description != "A tool-created skill for testing." {
		t.Errorf("expected description, got %q", s.Manifest.Description)
	}
	if len(s.Params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(s.Params))
	}
	if s.Params[0].Name != "city" || !s.Params[0].Required {
		t.Errorf("unexpected param[0]: %+v", s.Params[0])
	}
	if s.Params[1].Name != "units" || s.Params[1].Required {
		t.Errorf("unexpected param[1]: %+v", s.Params[1])
	}

	// Execute the loaded skill.
	result, err := ExecuteSkill(context.Background(), s.Manifest.Name, s.Source, json.RawMessage(`{"city":"Tokyo"}`))
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "weather in Tokyo" {
		t.Errorf("expected 'weather in Tokyo', got %q", result)
	}
}

func TestUnregister(t *testing.T) {
	registry := NewRegistry()
	registry.Register("temp_tool", func(ctx context.Context, args json.RawMessage) (string, error) {
		return "ok", nil
	}, ToolSchema{Name: "temp_tool"})

	if !registry.Has("temp_tool") {
		t.Fatal("expected temp_tool to be registered")
	}

	registry.Unregister("temp_tool")

	if registry.Has("temp_tool") {
		t.Error("expected temp_tool to be unregistered")
	}
}

func TestNewCreateSkill_Validation(t *testing.T) {
	registry := NewRegistry()
	skillsDir := t.TempDir()
	var grammarRebuilt bool
	rebuildGrammar := func() { grammarRebuilt = true }

	createSkill := NewCreateSkill(registry, skillsDir, rebuildGrammar)

	// Test case: failed test execution
	args := json.RawMessage(`{"name":"bad_skill","description":"fails","code":"throw new Error('fail');","test_args":"{}"}`)
	result, err := createSkill(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "failed test execution") {
		t.Errorf("expected failure message, got: %q", result)
	}
	if registry.Has("bad_skill") {
		t.Error("expected tool not to be registered")
	}

	// Test case: soft error
	args = json.RawMessage(`{"name":"bad_skill_2","description":"fails soft","code":"'Failed to connect to API'","test_args":"{}"}`)
	result, err = createSkill(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "failed test execution") {
		t.Errorf("expected soft failure message, got: %q", result)
	}
	if registry.Has("bad_skill_2") {
		t.Error("expected tool not to be registered on soft error")
	}

	// Test case: successful execution
	args = json.RawMessage(`{"name":"good_skill","description":"passes","code":"var x = 1; x;","test_args":"{}"}`)
	result, err = createSkill(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "created and registered successfully") {
		t.Errorf("expected success message, got: %q", result)
	}
	if !registry.Has("good_skill") {
		t.Error("expected tool to be registered")
	}
	if !grammarRebuilt {
		t.Error("expected grammar to be rebuilt")
	}
}
