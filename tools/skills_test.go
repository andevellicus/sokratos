package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func Test_validateSkillSource(t *testing.T) {
	// Valid JS.
	if err := validateSkillSource(`var x = 1 + 2; x;`); err != nil {
		t.Errorf("expected valid JS, got error: %v", err)
	}

	// Invalid JS.
	if err := validateSkillSource(`var x = {;`); err == nil {
		t.Error("expected error for invalid JS")
	}

	// Top-level return (should pass via IIFE wrapping).
	if err := validateSkillSource(`var x = 1 + 2; return x;`); err != nil {
		t.Errorf("expected return-based JS to pass via IIFE, got error: %v", err)
	}

	// Top-level return with function body.
	src := `var resp = http_request("GET", "https://example.com"); return resp.body;`
	if err := validateSkillSource(src); err != nil {
		t.Errorf("expected return-in-function-body to pass via IIFE, got error: %v", err)
	}
}

func TestExecuteSkill_ReturnStatement(t *testing.T) {
	source := `var result = args.a * args.b; return "product=" + result;`
	args := json.RawMessage(`{"a": 5, "b": 6}`)
	result, err := ExecuteSkill(context.Background(), "test_return", source, "", args, SkillDeps{})
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
	result, err := ExecuteSkill(context.Background(), "test", source, "", args, SkillDeps{})
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
	result, err := ExecuteSkill(context.Background(), "test", source, "", args, SkillDeps{})
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
	_, err := ExecuteSkill(context.Background(), "test", source, "", args, SkillDeps{})
	if err == nil {
		t.Fatal("expected ToolError for runtime error")
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("expected *ToolError, got %T: %v", err, err)
	}
	if te.Message == "" {
		t.Error("expected non-empty error message")
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
	RegisterSkill(registry, skill, SkillDeps{})

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

func Test_generateSkillMD(t *testing.T) {
	md := generateSkillMD("my_skill", "Does something useful.", "", []ParamSchema{
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
		"get_weather": {},
		"scan_feeds":  {},
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
		"get_weather": 1,
		"scan_feeds":  3,
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

func Test_generateSkillMD_RoundTripViaLoadSkills(t *testing.T) {
	// Simulate create_skill: generate SKILL.md + handler.js, then load via LoadSkills.
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test_generated")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	md := generateSkillMD("test_generated", "A tool-created skill for testing.", "", []ParamSchema{
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
	result, err := ExecuteSkill(context.Background(), s.Manifest.Name, s.Source, "", json.RawMessage(`{"city":"Tokyo"}`), SkillDeps{})
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

	createSkill := NewCreateSkill(registry, skillsDir, rebuildGrammar, SkillDeps{})

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

func TestExecuteSkill_ConsoleLog(t *testing.T) {
	source := `console.log("hello", "world"); console.warn("caution"); console.error("oops"); "done";`
	result, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if !strings.Contains(result, "done") {
		t.Errorf("expected result to contain 'done', got %q", result)
	}
	if !strings.Contains(result, "[LOG] hello world") {
		t.Errorf("expected log output, got %q", result)
	}
	if !strings.Contains(result, "[WARN] caution") {
		t.Errorf("expected warn output, got %q", result)
	}
	if !strings.Contains(result, "[ERROR] oops") {
		t.Errorf("expected error output, got %q", result)
	}
}

func TestExecuteSkill_BtoaAtob(t *testing.T) {
	source := `var encoded = btoa("hello world"); var decoded = atob(encoded); decoded;`
	result, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "hello world" {
		t.Errorf("expected 'hello world', got %q", result)
	}
}

func TestExecuteSkill_BtoaAtob_Value(t *testing.T) {
	source := `btoa("hello world");`
	result, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "aGVsbG8gd29ybGQ=" {
		t.Errorf("expected base64 'aGVsbG8gd29ybGQ=', got %q", result)
	}
}

func TestExecuteSkill_Sleep(t *testing.T) {
	start := time.Now()
	source := `sleep(100); "done";`
	result, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	elapsed := time.Since(start)
	if result != "done" {
		t.Errorf("expected 'done', got %q", result)
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("sleep(100) completed too fast: %v", elapsed)
	}
}

func TestExecuteSkill_SleepCapped(t *testing.T) {
	// sleep(999999) should be capped at 5 seconds, but we use a short
	// context deadline to make the test fast.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	source := `sleep(999999); "done";`
	_, _ = ExecuteSkill(ctx, "test", source, "", nil, SkillDeps{})
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Errorf("sleep should have been capped by context, took %v", elapsed)
	}
}

func TestExecuteSkill_Env(t *testing.T) {
	os.Setenv("SKILL_TEST_KEY", "test_value_123")
	defer os.Unsetenv("SKILL_TEST_KEY")

	source := `env("TEST_KEY");`
	result, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "test_value_123" {
		t.Errorf("expected 'test_value_123', got %q", result)
	}
}

func TestExecuteSkill_EnvBlocked(t *testing.T) {
	// Non-SKILL_ prefixed env vars should not be accessible.
	source := `var v = env("PATH"); typeof v === "undefined" ? "blocked" : "leaked";`
	result, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "blocked" {
		t.Errorf("expected 'blocked', got %q (env leaked non-SKILL_ var)", result)
	}
}

func TestExecuteSkill_HashSha256(t *testing.T) {
	source := `hash_sha256("hello");`
	result, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	// SHA-256 of "hello"
	expected := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if result != expected {
		t.Errorf("expected %s, got %q", expected, result)
	}
}

func TestExecuteSkill_HashHmacSha256(t *testing.T) {
	source := `hash_hmac_sha256("key", "message");`
	result, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	// HMAC-SHA256("key", "message")
	expected := "6e9ef29b75fffc5b7abae527d58fdadb2fe42e7219011976917343065f58ed4a"
	if result != expected {
		t.Errorf("expected %s, got %q", expected, result)
	}
}

func TestExecuteSkill_KV_NilPool(t *testing.T) {
	source := `kv_set("k", "v");`
	_, err := ExecuteSkill(context.Background(), "test", source, "", nil, SkillDeps{})
	if err == nil {
		t.Fatal("expected ToolError for nil pool")
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("expected *ToolError, got %T: %v", err, err)
	}
	if !strings.Contains(te.Message, "kv store unavailable") {
		t.Errorf("expected 'kv store unavailable' error, got %q", te.Message)
	}
}

// --- TypeScript transpilation tests ---

func TestTranspileTS(t *testing.T) {
	ts := `const greet = (name: string): string => "hello " + name; greet("world");`
	js, err := transpileTS(ts)
	if err != nil {
		t.Fatalf("transpileTS error: %v", err)
	}
	if !strings.Contains(js, "greet") {
		t.Errorf("expected transpiled JS to contain 'greet', got %q", js)
	}
	// Type annotations should be stripped.
	if strings.Contains(js, ": string") {
		t.Errorf("expected type annotations to be stripped, got %q", js)
	}
}

func TestTranspileTS_InvalidTS(t *testing.T) {
	ts := `const x: = ;`
	_, err := transpileTS(ts)
	if err == nil {
		t.Error("expected error for invalid TypeScript")
	}
}

func TestValidateTypeScriptSource(t *testing.T) {
	ts := `const add = (a: number, b: number): number => a + b; add(1, 2);`
	js, err := ValidateTypeScriptSource(ts)
	if err != nil {
		t.Fatalf("ValidateTypeScriptSource error: %v", err)
	}
	if js == "" {
		t.Error("expected non-empty transpiled JS")
	}
}

func TestLoadSkills_TypeScript(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "ts_skill")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	md := `---
name: ts_skill
language: typescript
description: A TypeScript test skill.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| name | string | yes |
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)
	os.WriteFile(filepath.Join(scriptsDir, "handler.ts"),
		[]byte(`const greet = (name: string): string => "hello " + name; greet(args.name);`), 0644)

	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("LoadSkills error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	s := skills[0]
	if s.Manifest.Name != "ts_skill" {
		t.Errorf("expected ts_skill, got %q", s.Manifest.Name)
	}
	if s.Manifest.Language != "typescript" {
		t.Errorf("expected language=typescript, got %q", s.Manifest.Language)
	}
	// Source should be transpiled JS (no type annotations).
	if strings.Contains(s.Source, ": string") {
		t.Errorf("expected transpiled JS without type annotations, got %q", s.Source)
	}
}

func TestExecuteSkill_TypeScript(t *testing.T) {
	// Simulate a TypeScript skill that's already been transpiled.
	ts := `const multiply = (a: number, b: number): number => a * b; multiply(args.a, args.b);`
	js, err := transpileTS(ts)
	if err != nil {
		t.Fatalf("transpileTS error: %v", err)
	}
	args := json.RawMessage(`{"a": 7, "b": 6}`)
	result, err := ExecuteSkill(context.Background(), "test_ts", js, "", args, SkillDeps{})
	if err != nil {
		t.Fatalf("ExecuteSkill error: %v", err)
	}
	if result != "42" {
		t.Errorf("expected 42, got %q", result)
	}
}

func Test_generateSkillMD_WithLanguage(t *testing.T) {
	md := generateSkillMD("ts_skill", "A TypeScript skill.", "typescript", []ParamSchema{
		{Name: "input", Type: "string", Required: true},
	})
	if !strings.Contains(md, "language: typescript") {
		t.Errorf("expected 'language: typescript' in frontmatter, got:\n%s", md)
	}

	// Round-trip: parse it back.
	manifest, params, err := parseSkillMD([]byte(md))
	if err != nil {
		t.Fatalf("round-trip parse error: %v", err)
	}
	if manifest.Language != "typescript" {
		t.Errorf("expected language=typescript, got %q", manifest.Language)
	}
	if manifest.Name != "ts_skill" {
		t.Errorf("expected name=ts_skill, got %q", manifest.Name)
	}
	if len(params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(params))
	}
}

func Test_generateSkillMD_JavaScriptDefault(t *testing.T) {
	// Empty language should not emit a language line.
	md := generateSkillMD("js_skill", "A JS skill.", "", nil)
	if strings.Contains(md, "language:") {
		t.Errorf("expected no language line for default JS, got:\n%s", md)
	}
}

func TestParseSkillMD_LanguageField(t *testing.T) {
	md := `---
name: typed_skill
language: typescript
description: A typed skill.
---
`
	manifest, _, err := parseSkillMD([]byte(md))
	if err != nil {
		t.Fatalf("parseSkillMD error: %v", err)
	}
	if manifest.Language != "typescript" {
		t.Errorf("expected language=typescript, got %q", manifest.Language)
	}
}

func TestManageSkills_TestAction(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "add_nums")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	md := `---
name: add_nums
description: Adds two numbers.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| a | number | yes |
| b | number | yes |
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)
	os.WriteFile(filepath.Join(scriptsDir, "handler.js"), []byte(`"sum=" + (args.a + args.b);`), 0644)

	registry := NewRegistry()
	rebuildGrammar := func() {}
	manage := NewManageSkills(registry, dir, rebuildGrammar, SkillDeps{})

	// Test with args.
	args := json.RawMessage(`{"action":"test","name":"add_nums","test_args":{"a":3,"b":4}}`)
	result, err := manage(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "sum=7") {
		t.Errorf("expected 'sum=7' in result, got %q", result)
	}

	// Test with no args (defaults to {}).
	args = json.RawMessage(`{"action":"test","name":"add_nums"}`)
	result, err = manage(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "test result") {
		t.Errorf("expected 'test result' in result, got %q", result)
	}
}

func TestManageSkills_TestAction_TypeScript(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "ts_adder")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	md := `---
name: ts_adder
language: typescript
description: Adds two numbers (TypeScript).
---
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)
	os.WriteFile(filepath.Join(scriptsDir, "handler.ts"),
		[]byte(`const add = (a: number, b: number): number => a + b; "result=" + add(args.a, args.b);`), 0644)

	registry := NewRegistry()
	manage := NewManageSkills(registry, dir, func() {}, SkillDeps{})

	args := json.RawMessage(`{"action":"test","name":"ts_adder","test_args":{"a":10,"b":20}}`)
	result, err := manage(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "result=30") {
		t.Errorf("expected 'result=30', got %q", result)
	}
	if !strings.Contains(result, "[ts]") {
		t.Errorf("expected '[ts]' tag in output, got %q", result)
	}
}

func TestManageSkills_TestAction_NotFound(t *testing.T) {
	dir := t.TempDir()
	registry := NewRegistry()
	manage := NewManageSkills(registry, dir, func() {}, SkillDeps{})

	args := json.RawMessage(`{"action":"test","name":"nonexistent"}`)
	result, err := manage(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "not found") {
		t.Errorf("expected 'not found' message, got %q", result)
	}
}

func TestLoadSkills_TypeScript_AutoDetect(t *testing.T) {
	// handler.ts on disk without explicit language in frontmatter — should auto-detect.
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "auto_ts")
	scriptsDir := filepath.Join(skillDir, "scripts")
	os.MkdirAll(scriptsDir, 0755)

	md := `---
name: auto_ts
description: Auto-detected TypeScript skill.
---
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644)
	os.WriteFile(filepath.Join(scriptsDir, "handler.ts"),
		[]byte(`const x: number = 42; x;`), 0644)

	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("LoadSkills error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Manifest.Language != "typescript" {
		t.Errorf("expected auto-detected language=typescript, got %q", skills[0].Manifest.Language)
	}
}
