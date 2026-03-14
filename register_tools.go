package main

import (
	"net/url"
	"strconv"

	"sokratos/config"
	"sokratos/db"
	"sokratos/pipelines"
	"sokratos/tools"
)

// coreDelegatableTools is the canonical list of built-in tools available
// for subagent delegation. Skills are appended dynamically by rebuildGrammar.
var coreDelegatableTools = []string{
	"search_email", "search_calendar", "search_memory", "save_memory",
	"search_web", "read_url", "run_command",
	"read_file", "write_file", "patch_file", "list_files",
	"create_skill", "manage_skills", "update_skill",
}

// collectInternalHosts extracts host:port pairs from configured service URLs
// for the skill HTTP bridge allowlist.
func collectInternalHosts(cfg *config.AppConfig) []string {
	var hosts []string
	for _, raw := range []string{cfg.SearxngURL, cfg.EmbedURL, cfg.RsshubURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if h := u.Host; h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// toolsBundle groups the outputs of registerTools to avoid a growing
// positional return signature.
type toolsBundle struct {
	Registry       *tools.Registry
	EmailTriageCfg *pipelines.TriageConfig
	DelegateConfig *tools.DelegateConfig
	ShellExec      *tools.ShellExec
}

func registerTools(cfg *config.AppConfig, svc *serviceBundle) *toolsBundle {
	registry := tools.NewRegistry()

	registerCoreTools(registry, svc.StateMgr)
	registerDBTools(registry, db.Pool, svc.InterruptChan, svc.Subagent)
	registerObjectiveTools(registry, db.Pool)
	registerAITools(registry, svc.DTC, db.Pool, cfg.EmbedURL, cfg.EmbedModel)

	if db.Pool != nil && cfg.EmbedURL != "" {
		registry.Register("search_memory", tools.NewSearchMemory(db.Pool, cfg.EmbedURL, cfg.EmbedModel, svc.Subagent, cfg.MemorySearchLimit), tools.ToolSchema{
			Name:          "search_memory",
			Description:   "Search long-term memory by keywords, tags, or date range",
			ProgressLabel: "Searching memory...",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: true},
				{Name: "tags", Type: "array", Required: false},
				{Name: "start_date", Type: "string", Required: false},
				{Name: "end_date", Type: "string", Required: false},
				{Name: "memory_type", Type: "string", Required: false},
			},
		})
		registry.Register("save_memory", tools.NewSaveMemory(db.Pool, cfg.EmbedURL, cfg.EmbedModel, svc.BgGrammarFunc, svc.GrammarFunc, svc.QueueFunc), tools.ToolSchema{
			Name:          "save_memory",
			Description:   "Save to long-term memory with salience scoring",
			ProgressLabel: "Saving to memory...",
			Params: []tools.ParamSchema{
				{Name: "summary", Type: "string", Required: true},
				{Name: "tags", Type: "array", Required: false},
				{Name: "category", Type: "string", Required: false},
				{Name: "salience_score", Type: "number", Required: false},
				{Name: "memory_type", Type: "string", Required: false},
			},
		})
		registry.Register("forget_topic", tools.NewForgetTopic(db.Pool, cfg.EmbedURL, cfg.EmbedModel), tools.ToolSchema{
			Name:        "forget_topic",
			Description: "Archive all memories related to a topic",
			Params: []tools.ParamSchema{
				{Name: "topic", Type: "string", Required: true},
				{Name: "confirm", Type: "boolean", Required: false},
			},
		})
	}
	// Build email triage config if dependencies are available.
	// TriageGrammar is left empty here and set after initLLM builds the grammar.
	var triageCfg *pipelines.TriageConfig
	if db.Pool != nil && cfg.EmbedURL != "" && svc.DTC != nil {
		triageCfg = &pipelines.TriageConfig{
			Pool:          db.Pool,
			EmbedEndpoint: cfg.EmbedURL,
			EmbedModel:    cfg.EmbedModel,
			DTC:           svc.DTC,
			QueueFn:       svc.QueueFunc,
			BgGrammarFn:   svc.BgGrammarFunc,
			RetryQueue:    svc.TriageRetryQueue,
			Metrics:       svc.Metrics,
		}
	}

	registerGmailTools(registry, db.Pool, triageCfg, cfg.EmailDisplayBatch, svc.Subagent)
	registerCalendarTools(registry, db.Pool)
	registerWebTools(registry, cfg.SearxngURL, svc.Subagent)

	registry.Register("run_code", tools.NewRunCode(), tools.ToolSchema{
		Name:        "run_code",
		Description: "Execute JavaScript code in a sandboxed ES5 runtime",
		Params: []tools.ParamSchema{
			{Name: "code", Type: "string", Required: true},
		},
	})

	// Register file I/O tools.
	foc := tools.NewFileOpsConfig(cfg.WorkspaceDir, svc.Platform, cfg.ConfirmationTimeout)
	registerFileTools(registry, foc)

	// Register prompt_user for interactive option selection.
	if svc.Platform != nil {
		// Derive channel from the first allowed Telegram ID (single-user bot).
		channelIDFn := func() string {
			for id := range cfg.AllowedIDs {
				return strconv.FormatInt(id, 10)
			}
			return ""
		}
		registry.Register("prompt_user", tools.NewPromptUser(svc.Platform, channelIDFn, cfg.ConfirmationTimeout), tools.ToolSchema{
			Name:        "prompt_user",
			Description: "Present a menu of options to the user and wait for their selection",
			Params: []tools.ParamSchema{
				{Name: "prompt", Type: "string", Required: true},
				{Name: "options", Type: "array", Required: true},
			},
		})
	}

	// Register shell command tool.
	shellExec := registerShellTool(registry, db.Pool, cfg)

	// Register delegate_task after all delegatable tools are available.
	delegateConfig := registerDelegateTask(registry, svc.Subagent)

	return &toolsBundle{
		Registry:       registry,
		EmailTriageCfg: triageCfg,
		DelegateConfig: delegateConfig,
		ShellExec:      shellExec,
	}
}
