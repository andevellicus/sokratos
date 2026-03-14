package skillrt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"sokratos/logger"
	"sokratos/toolreg"
)

// SkillManifest holds the parsed SKILL.md frontmatter fields.
type SkillManifest struct {
	Name          string `yaml:"name"`
	Description   string `yaml:"description"`
	Language      string `yaml:"language"`       // "javascript" (default) or "typescript"
	ProgressLabel string `yaml:"progress_label"` // shown in progress indicators
}

// Skill represents a fully loaded skill ready for registration.
type Skill struct {
	Manifest SkillManifest
	Params   []toolreg.ParamSchema
	Source   string // handler.js content
	Dir      string // skill directory (for config.txt loading)
}

// LoadSkills discovers and loads all skills from the given directory.
// Each skill lives in a subdirectory containing SKILL.md and scripts/handler.js.
// Returns nil if the directory doesn't exist. Logs warnings for malformed skills
// and continues loading others.
func LoadSkills(dir string) ([]Skill, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}

	pattern := filepath.Join(dir, "*", "SKILL.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob skills: %w", err)
	}

	var skills []Skill
	for _, mdPath := range matches {
		skillDir := filepath.Dir(mdPath)
		skillName := filepath.Base(skillDir)

		mdData, err := os.ReadFile(mdPath)
		if err != nil {
			logger.Log.Warnf("[skills] failed to read %s: %v", mdPath, err)
			continue
		}

		manifest, params, err := parseSkillMD(mdData)
		if err != nil {
			logger.Log.Warnf("[skills] failed to parse %s: %v", mdPath, err)
			continue
		}

		// Validate name matches directory.
		if manifest.Name != skillName {
			logger.Log.Warnf("[skills] name mismatch in %s: manifest=%q dir=%q", mdPath, manifest.Name, skillName)
			continue
		}

		source, lang, loadErr := loadSkillSource(skillDir)
		if loadErr != nil {
			logger.Log.Warnf("[skills] %s: %v", skillName, loadErr)
			continue
		}
		if manifest.Language == "" {
			manifest.Language = lang
		}

		skills = append(skills, Skill{
			Manifest: manifest,
			Params:   params,
			Source:   source,
			Dir:      skillDir,
		})
		logger.Log.Infof("[skills] loaded skill: %s", skillName)
	}

	return skills, nil
}

// loadSkillSource reads and compiles a skill's handler source from disk.
// Tries handler.ts (transpile) first, then handler.js. Returns (source, lang, error).
func loadSkillSource(dir string) (string, string, error) {
	tsPath := filepath.Join(dir, "scripts", "handler.ts")
	jsPath := filepath.Join(dir, "scripts", "handler.js")

	if tsData, tsErr := os.ReadFile(tsPath); tsErr == nil {
		transpiled, tErr := transpileTS(string(tsData))
		if tErr != nil {
			return "", "", fmt.Errorf("TS transpilation failed: %w", tErr)
		}
		if err := ValidateSkillSource(transpiled); err != nil {
			return "", "", fmt.Errorf("invalid transpiled JS: %w", err)
		}
		return transpiled, "typescript", nil
	} else if jsData, jsErr := os.ReadFile(jsPath); jsErr == nil {
		if err := ValidateSkillSource(string(jsData)); err != nil {
			return "", "", fmt.Errorf("invalid JS: %w", err)
		}
		return string(jsData), "javascript", nil
	} else {
		return "", "", fmt.Errorf("missing handler.ts/handler.js: %w", jsErr)
	}
}

// parseSkillMD parses a SKILL.md file into manifest fields and parameter schemas.
// It extracts YAML frontmatter (name, description) and the ## Parameters markdown table.
func parseSkillMD(data []byte) (SkillManifest, []toolreg.ParamSchema, error) {
	content := string(data)

	// Extract frontmatter between --- delimiters.
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return SkillManifest{}, nil, fmt.Errorf("missing YAML frontmatter delimiters")
	}
	frontmatter := parts[1]
	body := parts[2]

	manifest, err := parseFrontmatter(frontmatter)
	if err != nil {
		return SkillManifest{}, nil, err
	}

	params := parseParamsTable(body)

	return manifest, params, nil
}

// parseFrontmatter extracts name, description, language, and progress_label
// from YAML frontmatter using yaml.v3. Block scalars (e.g. description: |)
// are handled natively by the YAML parser.
func parseFrontmatter(fm string) (SkillManifest, error) {
	var m SkillManifest
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return m, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}

	// Trim trailing whitespace/newlines from block scalar descriptions.
	m.Description = strings.TrimSpace(m.Description)

	if m.Name == "" {
		return m, fmt.Errorf("missing required field: name")
	}
	if m.Description == "" {
		return m, fmt.Errorf("missing required field: description")
	}

	return m, nil
}

// parseParamsTable finds a ## Parameters heading and parses the markdown table
// beneath it into ParamSchema slices.
func parseParamsTable(body string) []toolreg.ParamSchema {
	lines := strings.Split(body, "\n")

	// Find ## Parameters heading.
	paramIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Parameters" {
			paramIdx = i
			break
		}
	}
	if paramIdx < 0 {
		return nil
	}

	// Skip heading, header row, and separator row, then parse data rows.
	var params []toolreg.ParamSchema
	started := false
	for i := paramIdx + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		// Stop at next heading.
		if strings.HasPrefix(line, "##") {
			break
		}

		// Skip the header row and separator row.
		if !started {
			if strings.Contains(line, "---") {
				started = true
			}
			continue
		}

		// Parse table row: | Name | Type | Required |
		cells := strings.Split(line, "|")
		var cleaned []string
		for _, c := range cells {
			c = strings.TrimSpace(c)
			if c != "" {
				cleaned = append(cleaned, c)
			}
		}
		if len(cleaned) < 3 {
			continue
		}

		name := cleaned[0]
		typ := strings.ToLower(cleaned[1])
		req := strings.ToLower(cleaned[2]) == "yes"

		// Normalize type to our supported set.
		switch typ {
		case "string", "number", "boolean", "array":
			// ok
		default:
			typ = "string"
		}

		params = append(params, toolreg.ParamSchema{
			Name:     name,
			Type:     typ,
			Required: req,
		})
	}

	return params
}

// generateSkillMD produces a SKILL.md file content from the given fields.
// When language is "typescript", a `language: typescript` line is emitted in
// the frontmatter. Empty or "javascript" omits the field (default).
func generateSkillMD(name, description, language string, params []toolreg.ParamSchema) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	if language == "typescript" {
		b.WriteString("language: typescript\n")
	}
	if strings.Contains(description, "\n") {
		b.WriteString("description: |\n")
		for _, line := range strings.Split(description, "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	} else {
		fmt.Fprintf(&b, "description: %s\n", description)
	}
	b.WriteString("---\n")

	if len(params) > 0 {
		b.WriteString("\n## Parameters\n\n")
		b.WriteString("| Name | Type | Required |\n")
		b.WriteString("|------|------|----------|\n")
		for _, p := range params {
			req := "no"
			if p.Required {
				req = "yes"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", p.Name, p.Type, req)
		}
	}

	return b.String()
}

// SyncSkills scans the skills directory, compares against the registry, and
// handles new, removed, and changed skills. It tracks SKILL.md mtimes in the
// provided cache to detect schema changes. Returns true if any changes were
// applied (and the grammar was rebuilt).
func SyncSkills(registry *toolreg.Registry, skillsDir string, rebuildGrammar GrammarRebuildFunc, mtimeCache map[string]time.Time, deps SkillDeps) bool {
	diskSkills, err := LoadSkills(skillsDir)
	if err != nil {
		logger.Log.Warnf("[skills] hot-reload: failed to load skills: %v", err)
		return false
	}

	// Index disk skills by name.
	onDisk := make(map[string]Skill, len(diskSkills))
	for _, s := range diskSkills {
		onDisk[s.Manifest.Name] = s
	}

	// Index registered skills by name.
	registered := make(map[string]struct{})
	for _, s := range registry.Schemas() {
		if s.IsSkill {
			registered[s.Name] = struct{}{}
		}
	}

	var added, removed, updated []string

	// Detect new and updated skills.
	for name, skill := range onDisk {
		mdPath := filepath.Join(skill.Dir, "SKILL.md")
		info, err := os.Stat(mdPath)
		if err != nil {
			continue
		}
		diskMtime := info.ModTime()

		if _, ok := registered[name]; !ok {
			// New skill on disk, not in registry.
			RegisterSkill(registry, skill, deps)
			mtimeCache[name] = diskMtime
			added = append(added, name)
		} else if cached, ok := mtimeCache[name]; !ok {
			// First sync after startup — just populate the cache.
			mtimeCache[name] = diskMtime
		} else if diskMtime.After(cached) {
			// SKILL.md changed — re-register.
			registry.Unregister(name)
			RegisterSkill(registry, skill, deps)
			mtimeCache[name] = diskMtime
			updated = append(updated, name)
		}
	}

	// Detect removed skills.
	for name := range registered {
		if _, ok := onDisk[name]; !ok {
			registry.Unregister(name)
			delete(mtimeCache, name)
			removed = append(removed, name)
		}
	}

	changed := len(added) + len(removed) + len(updated)
	if changed == 0 {
		return false
	}

	rebuildGrammar()
	logger.Log.Infof("[skills] hot-reload: added %v, removed %v, updated %v", added, removed, updated)
	return true
}
