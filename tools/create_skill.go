package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sokratos/logger"
)

// GrammarRebuildFunc is called after skill registration/deletion to update
// the GBNF grammar across all consumers (lb, engine, tool agent).
type GrammarRebuildFunc func()

var skillNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,48}$`)

// NewCreateSkill returns a ToolFunc that creates a new JavaScript skill on
// disk, registers it in the live registry, and rebuilds the grammar.
func NewCreateSkill(registry *Registry, skillsDir string, rebuildGrammar GrammarRebuildFunc) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Params      string          `json:"params"`
			Code        string          `json:"code"`
			TestArgs    json.RawMessage `json:"test_args"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("Invalid arguments: %v", err), nil
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
		var params []ParamSchema
		if strings.TrimSpace(a.Params) != "" {
			if err := json.Unmarshal([]byte(a.Params), &params); err != nil {
				return fmt.Sprintf("Invalid params JSON: %v", err), nil
			}
		}

		// Compile-check the JavaScript.
		if err := ValidateSkillSource(a.Code); err != nil {
			return fmt.Sprintf("JavaScript syntax error: %v", err), nil
		}

		// Normalize test_args: accept both JSON string and JSON object.
		// If it's a quoted string like "{\"key\":\"val\"}", unwrap it.
		// If it's an object like {"key":"val"}, use it directly.
		var testArgsRaw []byte
		if len(a.TestArgs) == 0 {
			testArgsRaw = []byte("{}")
		} else if a.TestArgs[0] == '"' {
			// It's a JSON string — unwrap to get the inner JSON.
			var s string
			if err := json.Unmarshal(a.TestArgs, &s); err != nil {
				return "Invalid JSON in test_args string", nil
			}
			testArgsRaw = []byte(s)
			if !json.Valid(testArgsRaw) {
				return "Invalid JSON inside test_args string value", nil
			}
		} else {
			// It's a JSON object — use directly.
			testArgsRaw = a.TestArgs
		}
		if !json.Valid(testArgsRaw) {
			return "Invalid JSON in test_args", nil
		}

		testResult, err := ExecuteSkill(ctx, a.Name, a.Code, testArgsRaw)
		if err != nil {
			return fmt.Sprintf("Skill failed test execution: %v", err), nil
		}

		lowerResult := strings.ToLower(strings.TrimSpace(testResult))
		if strings.Contains(lowerResult, "execution error") || strings.HasPrefix(lowerResult, "error") || strings.HasPrefix(lowerResult, "failed to") || strings.Contains(lowerResult, "failed to get") {
			return fmt.Sprintf("Skill failed test execution: %s", testResult), nil
		}

		// Write files to disk.
		skillDir := filepath.Join(skillsDir, a.Name)
		scriptsDir := filepath.Join(skillDir, "scripts")
		if err := os.MkdirAll(scriptsDir, 0755); err != nil {
			return "", fmt.Errorf("create skill directory: %w", err)
		}

		mdContent := GenerateSkillMD(a.Name, a.Description, params)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(mdContent), 0644); err != nil {
			return "", fmt.Errorf("write SKILL.md: %w", err)
		}

		if err := os.WriteFile(filepath.Join(scriptsDir, "handler.js"), []byte(a.Code), 0644); err != nil {
			return "", fmt.Errorf("write handler.js: %w", err)
		}

		// Register the skill in the live registry.
		skill := Skill{
			Manifest: SkillManifest{
				Name:        a.Name,
				Description: a.Description,
			},
			Params: params,
			Source: a.Code,
		}
		RegisterSkill(registry, skill)

		// Rebuild grammar so the subagent can produce valid JSON for this tool.
		rebuildGrammar()

		logger.Log.Infof("[skills] created skill: %s", a.Name)
		return fmt.Sprintf("Skill %q created and registered successfully. It is now available as a tool.", a.Name), nil
	}
}

// NewManageSkills returns a ToolFunc for listing and deleting skills.
func NewManageSkills(registry *Registry, skillsDir string, rebuildGrammar GrammarRebuildFunc) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Action string `json:"action"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("Invalid arguments: %v", err), nil
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
			logger.Log.Infof("[skills] deleted skill: %s", a.Name)
			return fmt.Sprintf("Skill %q deleted and unregistered.", a.Name), nil

		default:
			return fmt.Sprintf("Unknown action %q — use 'list' or 'delete'", a.Action), nil
		}
	}
}
