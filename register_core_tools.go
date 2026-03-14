package main

import (
	"sokratos/clients"
	"sokratos/config"
	"sokratos/engine"
	"sokratos/logger"
	"sokratos/tools"

	"github.com/jackc/pgx/v5/pgxpool"
)

func registerCoreTools(registry *tools.Registry, stateMgr *engine.StateManager) {
	registry.Register("update_state", tools.NewUpdateState(stateMgr), tools.ToolSchema{
		Name:        "update_state",
		Description: "Update your status and current task",
		Params: []tools.ParamSchema{
			{Name: "status", Type: "string", Required: true},
			{Name: "task", Type: "string", Required: true},
		},
	})
	registry.Register("set_preference", tools.NewSetPreference(stateMgr), tools.ToolSchema{
		Name:        "set_preference",
		Description: "Save a quick-access user preference (name, location, timezone, etc.)",
		Params: []tools.ParamSchema{
			{Name: "key", Type: "string", Required: true},
			{Name: "value", Type: "string", Required: true},
		},
	})
	registry.Register("reply_to_job", tools.NewReplyToJob(stateMgr), tools.ToolSchema{
		Name:        "reply_to_job",
		Description: "Send a message to a background Brain job that is waiting for input",
		Params: []tools.ParamSchema{
			{Name: "job_id", Type: "string", Required: true},
			{Name: "message", Type: "string", Required: true},
		},
	})
	registry.Register("cancel_job", tools.NewCancelJob(stateMgr), tools.ToolSchema{
		Name:        "cancel_job",
		Description: "Cancel an active background Brain job",
		Params: []tools.ParamSchema{
			{Name: "job_id", Type: "string", Required: true},
		},
	})
}

func registerWebTools(registry *tools.Registry, searxngURL string, sc *clients.SubagentClient) {
	if searxngURL != "" {
		registry.Register("search_web", tools.NewSearchWeb(searxngURL, sc), tools.ToolSchema{
			Name:          "search_web",
			Description:   "Search the internet via SearXNG",
			ProgressLabel: "Searching the web...",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: true},
				{Name: "max_results", Type: "number", Required: false},
			},
		})
	}
	registry.Register("read_url", tools.NewReadURL(), tools.ToolSchema{
		Name:          "read_url",
		Description:   "Fetch and extract text content from a URL",
		ProgressLabel: "Reading page...",
		Params: []tools.ParamSchema{
			{Name: "url", Type: "string", Required: true},
			{Name: "max_chars", Type: "number", Required: false},
		},
	})
}

func registerFileTools(registry *tools.Registry, foc *tools.FileOpsConfig) {
	registry.Register("read_file", tools.NewReadFile(foc), tools.ToolSchema{
		Name:        "read_file",
		Description: "Read a file from the workspace (returns numbered lines)",
		Params: []tools.ParamSchema{
			{Name: "path", Type: "string", Required: true},
			{Name: "offset", Type: "number", Required: false},
			{Name: "limit", Type: "number", Required: false},
		},
	})
	registry.Register("write_file", tools.NewWriteFile(foc), tools.ToolSchema{
		Name:        "write_file",
		Description: "Create or overwrite a file in the workspace",
		Params: []tools.ParamSchema{
			{Name: "path", Type: "string", Required: true},
			{Name: "content", Type: "string", Required: true},
		},
	})
	registry.Register("patch_file", tools.NewPatchFile(foc), tools.ToolSchema{
		Name:        "patch_file",
		Description: "Find and replace a unique string in a workspace file",
		Params: []tools.ParamSchema{
			{Name: "path", Type: "string", Required: true},
			{Name: "old_string", Type: "string", Required: true},
			{Name: "new_string", Type: "string", Required: true},
		},
	})
	registry.Register("list_files", tools.NewListFiles(foc), tools.ToolSchema{
		Name:        "list_files",
		Description: "List files in a workspace directory with optional glob pattern",
		Params: []tools.ParamSchema{
			{Name: "path", Type: "string", Required: true},
			{Name: "pattern", Type: "string", Required: false},
			{Name: "recursive", Type: "boolean", Required: false},
		},
	})
}

func registerShellTool(registry *tools.Registry, pool *pgxpool.Pool, cfg *config.AppConfig) *tools.ShellExec {
	se, err := tools.NewShellExec(pool, cfg.WorkspaceDir, ".config/shell.toml")
	if err != nil {
		logger.Log.Warnf("[startup] shell tool disabled: %v", err)
		return nil
	}
	registry.Register("run_command", se.ToolFunc(), tools.ToolSchema{
		Name:          "run_command",
		Description:   "Execute an allowlisted shell command (audited). " + se.CommandDescriptions(),
		ProgressLabel: "Running command...",
		Params: []tools.ParamSchema{
			{Name: "command", Type: "string", Required: true},
			{Name: "working_dir", Type: "string", Required: false},
		},
	})
	return se
}
