package skillrt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sokratos/logger"
	"sokratos/toolreg"
)

// GrammarRebuildFunc is called after skill registration/deletion to update
// the GBNF grammar across all consumers (lb, engine, tool agent).
type GrammarRebuildFunc func()

var skillNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,48}$`)

// normalizeTestArgs unwraps and validates test_args JSON from the LLM.
// Returns (raw bytes, soft error). Non-empty error means return early.
func normalizeTestArgs(raw json.RawMessage) ([]byte, string) {
	if len(raw) == 0 {
		return []byte("{}"), ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, "Invalid JSON in test_args string"
		}
		b := []byte(s)
		if !json.Valid(b) {
			return nil, "Invalid JSON inside test_args string value"
		}
		return b, ""
	}
	if !json.Valid(raw) {
		return nil, "Invalid JSON in test_args"
	}
	return raw, ""
}

// isTestFailure checks if a skill test result indicates failure.
func isTestFailure(result string) bool {
	lower := strings.ToLower(strings.TrimSpace(result))
	return strings.Contains(lower, "execution error") ||
		strings.HasPrefix(lower, "error") ||
		strings.HasPrefix(lower, "failed to") ||
		strings.Contains(lower, "failed to get")
}

// validateAndCompileSource validates and compiles skill source code.
// Returns (execSource, soft error string). Non-empty error means return early.
func validateAndCompileSource(lang, code string) (string, string) {
	if lang == "typescript" {
		transpiled, tErr := ValidateTypeScriptSource(code)
		if tErr != nil {
			return "", fmt.Sprintf("TypeScript error: %v", tErr)
		}
		return transpiled, ""
	}
	if err := ValidateSkillSource(code); err != nil {
		return "", fmt.Sprintf("JavaScript syntax error: %v", err)
	}
	return code, ""
}

// NewCreateSkill returns a ToolFunc that creates a new JavaScript skill on
// disk, registers it in the live registry, and rebuilds the grammar.
func NewCreateSkill(registry *toolreg.Registry, skillsDir string, rebuildGrammar GrammarRebuildFunc, deps SkillDeps) toolreg.ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Params      string          `json:"params"`
			Code        string          `json:"code"`
			Language    string          `json:"language"`
			TestArgs    json.RawMessage `json:"test_args"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}

		// Validate name.
		if !skillNameRe.MatchString(a.Name) {
			return "Invalid skill name: must be lowercase letters, digits, underscores, or dashes, 2-49 chars", nil
		}

		// Check for collision with existing tools.
		if registry.Has(a.Name) {
			return fmt.Sprintf("Tool %q already exists — choose a different name", a.Name), nil
		}

		// Validate required fields.
		if strings.TrimSpace(a.Description) == "" {
			return "Description is required", nil
		}
		if strings.TrimSpace(a.Code) == "" {
			return "Code is required", nil
		}

		// Parse params if provided.
		var params []toolreg.ParamSchema
		if strings.TrimSpace(a.Params) != "" {
			if err := json.Unmarshal([]byte(a.Params), &params); err != nil {
				return fmt.Sprintf("Invalid params JSON: %v", err), nil
			}
		}

		// Normalize language.
		lang := strings.ToLower(strings.TrimSpace(a.Language))
		switch lang {
		case "ts", "typescript":
			lang = "typescript"
		default:
			lang = "javascript"
		}

		// Compile-check the source (transpile if TypeScript).
		execSource, compileErr := validateAndCompileSource(lang, a.Code)
		if compileErr != "" {
			return compileErr, nil
		}

		// Normalize and validate test args.
		testArgsRaw, argsErr := normalizeTestArgs(a.TestArgs)
		if argsErr != "" {
			return argsErr, nil
		}

		testResult, err := ExecuteSkill(ctx, a.Name, execSource, "", testArgsRaw, deps)
		if err != nil {
			return fmt.Sprintf("Skill failed test execution: %v", err), nil
		}
		if isTestFailure(testResult) {
			return fmt.Sprintf("Skill failed test execution: %s", testResult), nil
		}

		// Write files to disk.
		skillDir := filepath.Join(skillsDir, a.Name)
		scriptsDir := filepath.Join(skillDir, "scripts")
		if err := os.MkdirAll(scriptsDir, 0755); err != nil {
			return "", fmt.Errorf("create skill directory: %w", err)
		}

		mdContent := generateSkillMD(a.Name, a.Description, lang, params)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(mdContent), 0644); err != nil {
			return "", fmt.Errorf("write SKILL.md: %w", err)
		}

		// Save as handler.ts for TypeScript, handler.js for JavaScript.
		handlerFile := "handler.js"
		if lang == "typescript" {
			handlerFile = "handler.ts"
		}
		if err := os.WriteFile(filepath.Join(scriptsDir, handlerFile), []byte(a.Code), 0644); err != nil {
			return "", fmt.Errorf("write %s: %w", handlerFile, err)
		}

		// Register the skill in the live registry. Source is always transpiled JS.
		skill := Skill{
			Manifest: SkillManifest{
				Name:        a.Name,
				Description: a.Description,
				Language:    lang,
			},
			Params: params,
			Source: execSource,
		}
		RegisterSkill(registry, skill, deps)

		// Rebuild grammar so the subagent can produce valid JSON for this tool.
		rebuildGrammar()

		logger.Log.Infof("[skills] created skill: %s", a.Name)
		return fmt.Sprintf("Skill %q created and registered successfully. It is now available as a tool.", a.Name), nil
	}
}

// NewManageSkills returns a ToolFunc for listing, deleting, and testing skills.
func NewManageSkills(registry *toolreg.Registry, skillsDir string, rebuildGrammar GrammarRebuildFunc, deps SkillDeps) toolreg.ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Action   string          `json:"action"`
			Name     string          `json:"name"`
			TestArgs json.RawMessage `json:"test_args"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}

		switch strings.ToLower(a.Action) {
		case "list":
			skills, err := LoadSkills(skillsDir)
			if err != nil {
				return fmt.Sprintf("Failed to list skills: %v", err), nil
			}
			if len(skills) == 0 {
				return "No skills installed.", nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Installed skills (%d):\n", len(skills))
			for _, s := range skills {
				fmt.Fprintf(&b, "- %s: %s", s.Manifest.Name, s.Manifest.Description)
				if s.Manifest.Language == "typescript" {
					b.WriteString(" [ts]")
				}
				if len(s.Params) > 0 {
					var pNames []string
					for _, p := range s.Params {
						pNames = append(pNames, p.Name)
					}
					fmt.Fprintf(&b, " [params: %s]", strings.Join(pNames, ", "))
				}
				b.WriteString("\n")
			}
			return b.String(), nil

		case "delete":
			if strings.TrimSpace(a.Name) == "" {
				return "Name is required for delete action", nil
			}
			skillDir := filepath.Join(skillsDir, a.Name)
			if _, err := os.Stat(skillDir); os.IsNotExist(err) {
				return fmt.Sprintf("Skill %q not found on disk", a.Name), nil
			}
			if err := os.RemoveAll(skillDir); err != nil {
				return "", fmt.Errorf("delete skill directory: %w", err)
			}
			registry.Unregister(a.Name)
			rebuildGrammar()
			// Clean up orphaned KV data for the deleted skill.
			if deps.Pool != nil {
				kvCtx, kvCancel := context.WithTimeout(context.Background(), TimeoutSkillKV)
				if _, err := deps.Pool.Exec(kvCtx, "DELETE FROM skill_kv WHERE skill_name=$1", a.Name); err != nil {
					logger.Log.Warnf("[skills] failed to clean KV for deleted skill %s: %v", a.Name, err)
				}
				kvCancel()
			}
			logger.Log.Infof("[skills] deleted skill: %s", a.Name)
			return fmt.Sprintf("Skill %q deleted and unregistered.", a.Name), nil

		case "test":
			if strings.TrimSpace(a.Name) == "" {
				return "Name is required for test action", nil
			}
			skillDir := filepath.Join(skillsDir, a.Name)
			if _, err := os.Stat(skillDir); os.IsNotExist(err) {
				return fmt.Sprintf("Skill %q not found on disk", a.Name), nil
			}

			// Load the skill fresh from disk (transpiles TS if needed).
			skills, err := LoadSkills(skillsDir)
			if err != nil {
				return fmt.Sprintf("Failed to load skills: %v", err), nil
			}
			var skill *Skill
			for i := range skills {
				if skills[i].Manifest.Name == a.Name {
					skill = &skills[i]
					break
				}
			}
			if skill == nil {
				return fmt.Sprintf("Skill %q found on disk but failed to load (check logs)", a.Name), nil
			}

			// Normalize test_args.
			var testArgsRaw json.RawMessage
			if len(a.TestArgs) == 0 {
				testArgsRaw = json.RawMessage(`{}`)
			} else {
				testArgsRaw = a.TestArgs
			}

			result, execErr := ExecuteSkill(ctx, a.Name, skill.Source, skill.Dir, testArgsRaw, deps)
			if execErr != nil {
				return fmt.Sprintf("Skill %q test error: %v", a.Name, execErr), nil
			}
			lang := ""
			if skill.Manifest.Language == "typescript" {
				lang = " [ts]"
			}
			return fmt.Sprintf("Skill %q%s test result:\n%s", a.Name, lang, result), nil

		default:
			return fmt.Sprintf("Unknown action %q — use 'list', 'delete', or 'test'", a.Action), nil
		}
	}
}

// NewUpdateSkill returns a ToolFunc that updates an existing skill's source
// code on disk. It preserves the existing manifest (description, params,
// language) and only replaces the handler source. The skill is validated,
// test-executed, and overwritten on disk on success. No re-register is needed
// because RegisterSkill closures re-read source from disk on each invocation.
func NewUpdateSkill(registry *toolreg.Registry, skillsDir string, rebuildGrammar GrammarRebuildFunc, deps SkillDeps) toolreg.ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Name     string          `json:"name"`
			Code     string          `json:"code"`
			TestArgs json.RawMessage `json:"test_args"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}

		if strings.TrimSpace(a.Name) == "" {
			return "name is required", nil
		}
		if strings.TrimSpace(a.Code) == "" {
			return "code is required", nil
		}

		// Verify skill exists on disk.
		skillDir := filepath.Join(skillsDir, a.Name)
		mdPath := filepath.Join(skillDir, "SKILL.md")
		mdData, err := os.ReadFile(mdPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Sprintf("Skill %q not found on disk", a.Name), nil
			}
			return fmt.Sprintf("Failed to read skill manifest: %v", err), nil
		}

		// Parse existing manifest to get language, description, params.
		manifest, _, err := parseSkillMD(mdData)
		if err != nil {
			return fmt.Sprintf("Failed to parse SKILL.md for %q: %v", a.Name, err), nil
		}

		lang := manifest.Language
		if lang == "" {
			lang = "javascript"
		}

		// Validate/transpile the new source.
		execSource, compileErr := validateAndCompileSource(lang, a.Code)
		if compileErr != "" {
			return compileErr, nil
		}

		// Normalize and validate test args.
		testArgsRaw, argsErr := normalizeTestArgs(a.TestArgs)
		if argsErr != "" {
			return argsErr, nil
		}

		// Test execution with the new source.
		testResult, err := ExecuteSkill(ctx, a.Name, execSource, skillDir, testArgsRaw, deps)
		if err != nil {
			return fmt.Sprintf("Skill failed test execution: %v", err), nil
		}
		if isTestFailure(testResult) {
			return fmt.Sprintf("Skill failed test execution: %s", testResult), nil
		}

		// Overwrite handler on disk.
		handlerFile := "handler.js"
		if lang == "typescript" {
			handlerFile = "handler.ts"
		}
		scriptsDir := filepath.Join(skillDir, "scripts")
		if err := os.WriteFile(filepath.Join(scriptsDir, handlerFile), []byte(a.Code), 0644); err != nil {
			return "", fmt.Errorf("write %s: %w", handlerFile, err)
		}

		logger.Log.Infof("[skills] updated skill: %s", a.Name)
		return fmt.Sprintf("Skill %q updated. Test result:\n%s", a.Name, testResult), nil
	}
}
