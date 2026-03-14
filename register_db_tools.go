package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/routines"
	"sokratos/tools"
)

func registerObjectiveTools(registry *tools.Registry, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	registry.Register("manage_objectives", tools.NewManageObjectives(pool), tools.ToolSchema{
		Name:        "manage_objectives",
		Description: "Create, update, pause, resume, complete, retire, or list objectives",
		Params: []tools.ParamSchema{
			{Name: "op", Type: "string", Required: true},
			{Name: "summary", Type: "string", Required: false},
			{Name: "objective_id", Type: "number", Required: false},
			{Name: "priority", Type: "string", Required: false},
			{Name: "notes", Type: "string", Required: false},
		},
	})
}

func registerDBTools(registry *tools.Registry, pool *pgxpool.Pool, interruptChan chan struct{}, subagent *clients.SubagentClient) {
	if pool == nil {
		return
	}
	registry.Register("add_task", tools.NewAddTask(pool, interruptChan), tools.ToolSchema{
		Name:        "add_task",
		Description: "Add a scheduled task with optional due date and recurrence",
		Params: []tools.ParamSchema{
			{Name: "task", Type: "string", Required: true},
			{Name: "due_at", Type: "string", Required: false},
			{Name: "recur", Type: "string", Required: false},
		},
	})
	registry.Register("complete_task", tools.NewCompleteTask(pool, interruptChan), tools.ToolSchema{
		Name:        "complete_task",
		Description: "Mark current task done, advance queue",
		Params:      []tools.ParamSchema{{Name: "task_id", Type: "number", Required: false}},
	})
	registry.Register("manage_routines", tools.NewManageRoutines(pool, &routines.FileAdapter{Path: ".config/routines.toml"}), tools.ToolSchema{
		Name:        "manage_routines",
		Description: "Create, update, or delete autonomous routines",
		Params: []tools.ParamSchema{
			{Name: "op", Type: "string", Required: true},
			{Name: "name", Type: "string", Required: true},
			{Name: "interval", Type: "string", Required: false},
			{Name: "schedule", Type: "string", Required: false},
			{Name: "action", Type: "string", Required: false},
			{Name: "actions", Type: "array", Required: false},
			{Name: "action_args", Type: "object", Required: false},
			{Name: "goal", Type: "string", Required: false},
			{Name: "silent_if_empty", Type: "boolean", Required: false},
			{Name: "instruction", Type: "string", Required: false},
		},
	})
	if subagent != nil {
		registry.Register("ask_database", tools.NewAskDatabase(pool, subagent), tools.ToolSchema{
			Name:        "ask_database",
			Description: "Query the database using natural language (translated to SQL)",
			Params:      []tools.ParamSchema{{Name: "natural_language_query", Type: "string", Required: true}},
		})
	}
	registry.Register("query_metrics", tools.NewQueryMetrics(pool), tools.ToolSchema{
		Name:        "query_metrics",
		Description: "View system performance metrics (slots, dispatch, latency, tools, routines)",
		Params: []tools.ParamSchema{
			{Name: "report", Type: "string", Required: false},
			{Name: "window", Type: "string", Required: false},
		},
	})
}
