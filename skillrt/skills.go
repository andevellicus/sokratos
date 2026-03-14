package skillrt

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/logger"
	"sokratos/toolreg"
)

// SkillDeps groups optional dependencies for skill execution.
type SkillDeps struct {
	Pool     *pgxpool.Pool
	Registry *toolreg.Registry
	SC       *clients.SubagentClient
	DC       *toolreg.DelegateConfig
}

// RegisterSkill creates a ToolFunc closure wrapping ExecuteSkill and registers
// the skill in the tool registry.
func RegisterSkill(registry *toolreg.Registry, skill Skill, deps SkillDeps) {
	name := skill.Manifest.Name
	source := skill.Source
	dir := skill.Dir

	fn := func(ctx context.Context, args json.RawMessage) (string, error) {
		currentSource := source
		if dir != "" {
			if reloaded, _, err := loadSkillSource(dir); err == nil {
				currentSource = reloaded
			} else {
				logger.Log.Warnf("[skills] %s: live-reload failed, using cached source: %v", name, err)
			}
		}
		return ExecuteSkill(ctx, name, currentSource, dir, args, deps)
	}

	progressLabel := skill.Manifest.ProgressLabel
	if progressLabel == "" && skill.Manifest.Description != "" {
		progressLabel = skill.Manifest.Description + "..."
	}

	schema := toolreg.ToolSchema{
		Name:          name,
		Params:        skill.Params,
		Description:   skill.Manifest.Description,
		ProgressLabel: progressLabel,
		IsSkill:       true,
	}

	registry.Register(name, fn, schema)
}
