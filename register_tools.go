package main

import (
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/google"
	"sokratos/grammar"
	"sokratos/logger"
	"sokratos/pipelines"
	"sokratos/routines"
	"sokratos/tools"
)

// --- Tool Registration (grouped by domain) ---

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
	registry.Register("manage_routines", tools.NewManageRoutines(pool, &routines.FileAdapter{Path: "routines.toml"}), tools.ToolSchema{
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
}

// registerAITools is a no-op placeholder. In two-model mode, the Brain IS
// the deep thinker — consult_deep_thinker was removed to prevent deadlock
// (orchestrator holds the DTC sem, then DTC.Complete tries to re-acquire it).
func registerAITools(_ *tools.Registry, _ *clients.DeepThinkerClient, _ *pgxpool.Pool, _, _ string) {
}

// registerDelegateTask registers delegate_task AFTER all delegatable tools
// are already registered so the grammar is built with their schemas. Core
// tools (search_email, search_calendar, search_memory, save_memory) are
// always available; user-created skills are added dynamically via
// rebuildGrammar. Returns the DelegateConfig for live updates.
func registerDelegateTask(registry *tools.Registry, subagent *clients.SubagentClient) *tools.DelegateConfig {
	if subagent == nil {
		return nil
	}
	coreTools := []string{"search_email", "search_calendar", "search_memory", "save_memory", "search_web", "read_url"}
	schemas := registry.SchemasForTools(coreTools)
	g := grammar.BuildSubagentToolGrammar(schemas)
	dc := tools.NewDelegateConfig(coreTools, g)
	registry.Register("delegate_task", tools.NewDelegateTask(subagent, registry, dc), tools.ToolSchema{
		Name:        "delegate_task",
		Description: "Delegate a read-only task to a lightweight subagent",
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
}

func registerPlanTools(registry *tools.Registry, dtc *clients.DeepThinkerClient,
	subagent *clients.SubagentClient, dc *tools.DelegateConfig,
	wt *tools.WorkTracker) {

	if dtc == nil || subagent == nil || dc == nil {
		logger.Log.Warn("[startup] plan_and_execute disabled: missing dtc, subagent, or delegate config")
		return
	}

	registry.Register("plan_and_execute", tools.NewPlanAndExecute(dtc, subagent, dc, registry, wt), tools.ToolSchema{
		Name:        "plan_and_execute",
		Description: "Decompose and execute complex multi-step tasks (background=true for async)",
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

func registerTools(cfg *config.AppConfig, svc *serviceBundle) (*tools.Registry, *pipelines.TriageConfig, *tools.DelegateConfig) {
	registry := tools.NewRegistry()

	registerCoreTools(registry, svc.StateMgr)
	registerDBTools(registry, db.Pool, svc.InterruptChan, svc.Subagent)
	registerAITools(registry, svc.DTC, db.Pool, cfg.EmbedURL, cfg.EmbedModel)

	if db.Pool != nil && cfg.EmbedURL != "" {
		registry.Register("search_memory", tools.NewSearchMemory(db.Pool, cfg.EmbedURL, cfg.EmbedModel, svc.Subagent, cfg.MemorySearchLimit), tools.ToolSchema{
			Name:        "search_memory",
			Description: "Search long-term memory by keywords, tags, or date range",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: true},
				{Name: "tags", Type: "array", Required: false},
				{Name: "start_date", Type: "string", Required: false},
				{Name: "end_date", Type: "string", Required: false},
				{Name: "memory_type", Type: "string", Required: false},
			},
		})
		registry.Register("save_memory", tools.NewSaveMemory(db.Pool, cfg.EmbedURL, cfg.EmbedModel, svc.BgGrammarFunc, svc.GrammarFunc, svc.QueueFunc), tools.ToolSchema{
			Name:        "save_memory",
			Description: "Save to long-term memory with salience scoring",
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
	var emailTriageCfg *pipelines.TriageConfig
	if db.Pool != nil && cfg.EmbedURL != "" && svc.DTC != nil {
		emailTriageCfg = &pipelines.TriageConfig{
			Pool:          db.Pool,
			EmbedEndpoint: cfg.EmbedURL,
			EmbedModel:    cfg.EmbedModel,
			DTC:           svc.DTC,
			QueueFn:       svc.QueueFunc,
			BgGrammarFn:   svc.BgGrammarFunc,
			RetryQueue:    svc.TriageRetryQueue,
		}
	}

	registerGmailTools(registry, db.Pool, emailTriageCfg, cfg.EmailDisplayBatch)
	registerCalendarTools(registry, db.Pool)
	registerWebTools(registry, cfg.SearxngURL)

	registry.Register("run_code", tools.NewRunCode(), tools.ToolSchema{
		Name:        "run_code",
		Description: "Execute JavaScript code in a sandboxed ES5 runtime",
		Params: []tools.ParamSchema{
			{Name: "code", Type: "string", Required: true},
		},
	})

	// Register delegate_task after all delegatable tools are available.
	delegateConfig := registerDelegateTask(registry, svc.Subagent)

	return registry, emailTriageCfg, delegateConfig
}

// registerGmailTools registers tools for searching and interacting with Gmail.
func registerGmailTools(registry *tools.Registry, pool *pgxpool.Pool, triageCfg *pipelines.TriageConfig, emailDisplayBatch int) {
	if google.GmailService == nil {
		return
	}
	registry.Register("search_email", tools.NewSearchEmail(google.GmailService, pool, triageCfg, emailDisplayBatch), tools.ToolSchema{
		Name:        "search_email",
		Description: "Search Gmail inbox with optional time bounds",
		Params: []tools.ParamSchema{
			{Name: "query", Type: "string", Required: false},
			{Name: "time_min", Type: "string", Required: false},
			{Name: "time_max", Type: "string", Required: false},
			{Name: "max_results", Type: "number", Required: false},
		},
	})
	registry.Register("send_email", tools.NewSendEmail(google.GmailService), tools.ToolSchema{
		Name:        "send_email",
		Description: "Send a plain-text email",
		Params: []tools.ParamSchema{
			{Name: "to", Type: "string", Required: true},
			{Name: "subject", Type: "string", Required: true},
			{Name: "body", Type: "string", Required: true},
		},
	})
}

// registerCalendarTools registers tools for searching and creating calendar events.
func registerCalendarTools(registry *tools.Registry, pool *pgxpool.Pool) {
	if google.CalendarService == nil {
		return
	}
	registry.Register("search_calendar", tools.NewSearchCalendar(google.CalendarService, pool), tools.ToolSchema{
		Name:        "search_calendar",
		Description: "Search Google Calendar for events with optional time bounds",
		Params: []tools.ParamSchema{
			{Name: "query", Type: "string", Required: false},
			{Name: "time_min", Type: "string", Required: false},
			{Name: "time_max", Type: "string", Required: false},
			{Name: "max_results", Type: "number", Required: false},
		},
	})
	registry.Register("create_event", tools.NewCreateEvent(google.CalendarService), tools.ToolSchema{
		Name:        "create_event",
		Description: "Create a Google Calendar event. Use the user's local timezone offset in start/end times (e.g. 2026-03-07T19:00:00-05:00), NOT Z/UTC.",
		Params: []tools.ParamSchema{
			{Name: "title", Type: "string", Required: true},
			{Name: "start", Type: "string", Required: true},
			{Name: "end", Type: "string", Required: false},
			{Name: "description", Type: "string", Required: false},
			{Name: "location", Type: "string", Required: false},
			{Name: "attendees", Type: "array", Required: false},
		},
	})
}

func registerWebTools(registry *tools.Registry, searxngURL string) {
	if searxngURL != "" {
		registry.Register("search_web", tools.NewSearchWeb(searxngURL), tools.ToolSchema{
			Name:        "search_web",
			Description: "Search the internet via SearXNG",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: true},
				{Name: "max_results", Type: "number", Required: false},
			},
		})
	}
	registry.Register("read_url", tools.NewReadURL(), tools.ToolSchema{
		Name:        "read_url",
		Description: "Fetch and extract text content from a URL",
		Params: []tools.ParamSchema{
			{Name: "url", Type: "string", Required: true},
			{Name: "max_chars", Type: "number", Required: false},
		},
	})
}
