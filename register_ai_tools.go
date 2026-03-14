package main

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/grammar"
	"sokratos/logger"
	"sokratos/tools"
)

// registerAITools registers the deep reasoning tool. With the 9B as the default
// orchestrator, there's no semaphore deadlock — the 9B doesn't hold the DTC sem.
func registerAITools(registry *tools.Registry, dtc *clients.DeepThinkerClient, pool *pgxpool.Pool, embedURL, embedModel string) {
	if dtc == nil {
		return
	}
	registry.Register("deep_think", tools.NewDeepThink(dtc, pool, embedURL, embedModel), tools.ToolSchema{
		Name:          "deep_think",
		Description:   "Send a complex problem to the deep reasoning model (122B Brain) for thorough analysis. Use background=true for tasks requiring tool access. REQUIRED for skill creation: deep_think(background=true, task_type=\"create_skill\").",
		ProgressLabel: "Let me think about that....",
		Params: []tools.ParamSchema{
			{Name: "problem_statement", Type: "string", Required: true},
			{Name: "background", Type: "boolean", Required: false},
			{Name: "task_type", Type: "string", Required: false},
		},
	})
}

// registerDelegateTask registers delegate_task AFTER all delegatable tools
// are already registered so the grammar is built with their schemas.
// Returns the DelegateConfig for live updates.
func registerDelegateTask(registry *tools.Registry, subagent *clients.SubagentClient) *tools.DelegateConfig {
	if subagent == nil {
		return nil
	}
	coreTools := coreDelegatableTools
	schemas := registry.SchemasForTools(coreTools)
	g := grammar.BuildSubagentToolGrammar(schemas)
	dc := tools.NewDelegateConfig(coreTools, g)
	registry.Register("delegate_task", tools.NewDelegateTask(subagent, registry, dc), tools.ToolSchema{
		Name:          "delegate_task",
		Description:   "Delegate a read-only task to a lightweight subagent",
		ProgressLabel: "Working on it...",
		Params: []tools.ParamSchema{
			{Name: "directive", Type: "string", Required: true},
			{Name: "context", Type: "string", Required: false},
		},
	})
	return dc
}

func registerSkillTools(registry *tools.Registry, skillsDir string, rebuildGrammar tools.GrammarRebuildFunc, deps tools.SkillDeps) {
	skills, err := tools.LoadSkills(skillsDir)
	if err != nil {
		logger.Log.Warnf("Failed to load skills: %v", err)
	}
	for _, skill := range skills {
		tools.RegisterSkill(registry, skill, deps)
	}
	registry.Register("create_skill", tools.NewCreateSkill(registry, skillsDir, rebuildGrammar, deps), tools.ToolSchema{
		Name:        "create_skill",
		Description: "Create a new JavaScript or TypeScript skill registered as a live tool",
		Params: []tools.ParamSchema{
			{Name: "name", Type: "string", Required: true},
			{Name: "description", Type: "string", Required: true},
			{Name: "params", Type: "string", Required: false},
			{Name: "code", Type: "string", Required: true},
			{Name: "language", Type: "string", Required: false},
			{Name: "test_args", Type: "string", Required: true},
		},
		ConfirmFormat: func(args json.RawMessage) string {
			var a struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			_ = json.Unmarshal(args, &a)
			return fmt.Sprintf("⚠️ Create skill %q\n%s", a.Name, a.Description)
		},
		ConfirmCacheKey: func(args json.RawMessage) string {
			var a struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(args, &a) == nil && a.Name != "" {
				return "create_skill:" + a.Name
			}
			return "create_skill"
		},
	})
	registry.Register("manage_skills", tools.NewManageSkills(registry, skillsDir, rebuildGrammar, deps), tools.ToolSchema{
		Name:        "manage_skills",
		Description: "List, delete, or test installed skills",
		Params: []tools.ParamSchema{
			{Name: "action", Type: "string", Required: true},
			{Name: "name", Type: "string", Required: false},
			{Name: "test_args", Type: "string", Required: false},
		},
	})
	registry.Register("update_skill", tools.NewUpdateSkill(registry, skillsDir, rebuildGrammar, deps), tools.ToolSchema{
		Name:        "update_skill",
		Description: "Update an existing skill's source code (validates and tests before saving)",
		Params: []tools.ParamSchema{
			{Name: "name", Type: "string", Required: true},
			{Name: "code", Type: "string", Required: true},
			{Name: "test_args", Type: "string", Required: true},
		},
	})
}

func registerPlanTools(registry *tools.Registry, dtc *clients.DeepThinkerClient,
	subagent *clients.SubagentClient, dc *tools.DelegateConfig,
	wt *tools.WorkTracker) {

	if dtc == nil || subagent == nil || dc == nil {
		logger.Log.Warn("[startup] plan_and_execute disabled: missing dtc, subagent, or delegate config")
		return
	}

	planDeps := tools.PlanExecDeps{SC: subagent, DTC: dtc, DC: dc, Registry: registry}
	registry.Register("plan_and_execute", tools.NewPlanAndExecute(planDeps, wt), tools.ToolSchema{
		Name:          "plan_and_execute",
		Description:   "Decompose and execute complex multi-step tasks (background=true for async). NOT for skill creation — use deep_think(background=true, task_type=\"create_skill\") instead.",
		ProgressLabel: "Working on it...",
		Params: []tools.ParamSchema{
			{Name: "directive", Type: "string", Required: true},
			{Name: "context", Type: "string", Required: false},
			{Name: "background", Type: "boolean", Required: false},
			{Name: "priority", Type: "number", Required: false},
		},
	})

	if wt != nil {
		registry.Register("check_background_task", tools.NewCheckBackgroundTask(wt), tools.ToolSchema{
			Name:        "check_background_task",
			Description: "Check status, list, or cancel work items (background, routine, scheduled)",
			Params: []tools.ParamSchema{
				{Name: "task_id", Type: "number", Required: false},
				{Name: "action", Type: "string", Required: false},
			},
		})
	}
}
