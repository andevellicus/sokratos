package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/grammar"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/pipelines"
	"sokratos/prompts"
	"sokratos/routines"
	"sokratos/textutil"
	"sokratos/timeouts"
	"sokratos/tools"
)

// --- Engine Initialization ---

func initEngine(cfg *config.AppConfig, svc *serviceBundle, lb *llmBundle, registry *tools.Registry) *engine.Engine {
	var mu sync.Mutex

	eng := &engine.Engine{
		LLM: engine.LLMConfig{
			Client:           lb.Client,
			Model:            cfg.LLMModel,
			ToolAgent:        lb.ToolAgent,
			MaxToolResultLen: cfg.MaxToolResultLen,
			MaxWebSources:    cfg.MaxWebSources,
		},
		Cognitive: engine.CognitiveConfig{
			BufferThreshold:           cfg.CognitiveBufferThreshold,
			LullDuration:              cfg.LullDuration,
			Ceiling:                   cfg.CognitiveCeiling,
			ReflectionMemoryThreshold: cfg.ReflectionMemoryThreshold,
			ReflectionPrompt:          strings.TrimSpace(prompts.Reflection),
		},
		ToolExec:               registry.Execute,
		Mu:                     &mu,
		Interval:               cfg.HeartbeatInterval,
		RoutineInterval:        cfg.RoutineInterval,
		RoutineTimeout:         cfg.RoutineTimeout,
		SM:                     svc.StateMgr,
		DB:                     db.Pool,
		EmbedEndpoint:          cfg.EmbedURL,
		EmbedModel:             cfg.EmbedModel,
		MaxMessages:            40,
		MaintenanceInterval: cfg.MaintenanceInterval,
		TTL: engine.TTLConfig{
			MemoryStalenessDays:    cfg.MemoryStalenessDays,
			WorkItemsTTLDays:       cfg.WorkItemsTTLDays,
			ProcessedEmailsTTLDays: cfg.ProcessedEmailsTTLDays,
			ProcessedEventsTTLDays: cfg.ProcessedEventsTTLDays,
			FailedOpsTTLDays:       cfg.FailedOpsTTLDays,
			SkillKVTTLDays:         cfg.SkillKVTTLDays,
			ShellHistoryTTLDays:    cfg.ShellHistoryTTLDays,
		},
		Notifier: &notifierAdapter{bot: svc.Bot, allowedIDs: cfg.AllowedIDs},
		InterruptChan: svc.InterruptChan,
		Gatekeeper:    svc.Subagent,
		Memory: engine.MemoryFuncs{
			DTCQueueFn:  svc.DTCQueueFunc,
			SubagentFn:  svc.SubagentFunc,
			GrammarFn:   svc.GrammarFunc,
			BgGrammarFn: svc.BgGrammarFunc,
			QueueFn:     svc.QueueFunc,
		},
	}

	// Wire configurable cooldowns.
	eng.ObjectivePursuitCooldown = cfg.ObjectivePursuitCooldown
	eng.ObjectiveInferenceCooldown = cfg.ObjectiveInferenceCooldown
	eng.CuriosityCooldown = cfg.CuriosityCooldown
	eng.CuriositySignals = make(chan engine.CuriositySignal, 10)

	// Defer initial consolidation until after the first heartbeat tick so
	// Qwen3.5-27B is available for interactive requests during startup.
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		consolidateDeps := pipelines.PipelineDeps{
			Pool: db.Pool, DTC: svc.DTC,
			EmbedEndpoint: cfg.EmbedURL, EmbedModel: cfg.EmbedModel,
			GrammarFn: svc.GrammarFunc,
		}
		eng.OnFirstTick = func() {
			pipelines.CleanupPreTriageMemories(db.Pool)
			pipelines.RunInitialConsolidation(consolidateDeps, cfg.ConsolidationMemoryLimit)
			eng.RefreshProfile()
			eng.RefreshPersonality()
		}
	}

	return eng
}

// wireResult bundles the outputs of wireEngine that main() needs.
type wireResult struct {
	router         engine.SlotRouter
	rebuildGrammar func()
	skillMtimes    map[string]time.Time
	skillDeps      tools.SkillDeps
}

// wireEngine performs all post-construction wiring of the engine: slot router,
// work tracker, late tool registration, grammar rebuild, hot-reload, reflection
// sink, and cognitive services.
func wireEngine(
	cfg *config.AppConfig,
	svc *serviceBundle,
	lb *llmBundle,
	eng *engine.Engine,
	registry *tools.Registry,
	emailTriageCfg *pipelines.TriageConfig,
	delegateConfig *tools.DelegateConfig,
	shellExec *tools.ShellExec,
) wireResult {
	// Wire paradigm shift fast-path: after a paradigm shift is detected in
	// triage, run mini-consolidation then refresh the engine's profile state.
	if emailTriageCfg != nil {
		emailTriageCfg.ProfileRefreshFunc = func() {
			eng.RefreshProfile()
			eng.RefreshPersonality()
		}
	}

	// Slot router: in two-model mode, route orchestrator calls to 9B (primary)
	// with 122B Brain as fallback when all 9B slots are busy.
	var router engine.SlotRouter
	if cfg.BrainURL != "" && svc.DTC != nil && svc.Subagent != nil {
		brainClient := llm.NewClient(cfg.BrainURL)
		router = engine.NewSlotRouter(
			lb.Client, cfg.LLMModel, // 9B (primary orchestrator)
			brainClient, cfg.BrainModel, // 122B Brain (fallback)
			svc.Subagent, // 9B slots (cap 3)
			svc.DTC,      // Brain slots (cap 1)
		)
		eng.Router = router
		logger.Log.Infof("[startup] slot router initialized: 9B orchestrator + Brain fallback")
	}

	// Work tracker: unified tracking for background plans, routines, and scheduled tasks.
	var workTracker *tools.WorkTracker
	if db.Pool != nil {
		workTracker = tools.NewWorkTracker(db.Pool, func(text string) {
			if eng.Notifier != nil {
				eng.Notifier.Send(text)
			}
		})
		workTracker.OnComplete = func(directive, status string) {
			summary := fmt.Sprintf("Background task %s: %s", status, textutil.Truncate(directive, 80))
			eng.RecordBackgroundCompletion("background_task", summary)
		}
		workTracker.OnObjectiveTaskComplete = buildObjectiveTaskCallback(db.Pool, svc.Subagent, eng)
		workTracker.ShareGate = buildShareGate(svc.Subagent, eng, cfg.MaxDailyShares)
		workTracker.CleanupOrphans()
		workTracker.CleanupOldTasks()
		eng.WorkMonitor = workTracker
	}

	// Register tools that need the engine for refresh callbacks.
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		registry.Register("consolidate_memory", pipelines.NewConsolidateMemory(
			pipelines.PipelineDeps{
				Pool: db.Pool, DTC: svc.DTC,
				EmbedEndpoint: cfg.EmbedURL, EmbedModel: cfg.EmbedModel,
				GrammarFn: svc.GrammarFunc,
			}, cfg.ConsolidationMemoryLimit, func() {
				eng.RefreshProfile()
				eng.RefreshPersonality()
			},
		), tools.ToolSchema{
			Name:        "consolidate_memory",
			Description: "Trigger memory consolidation and profile update",
		})
	}
	if db.Pool != nil {
		registry.Register("manage_personality", tools.NewManagePersonality(db.Pool, func() {
			eng.RefreshPersonality()
		}), tools.ToolSchema{
			Name:        "manage_personality",
			Description: "View and evolve personality traits (set/remove/list)",
			Params: []tools.ParamSchema{
				{Name: "action", Type: "string", Required: true},
				{Name: "category", Type: "string", Required: false},
				{Name: "key", Type: "string", Required: false},
				{Name: "value", Type: "string", Required: false},
				{Name: "context", Type: "string", Required: false},
			},
		})
	}

	// Build grammar rebuild callback capturing registry + lb + eng.
	rebuildGrammar := func() {
		// Rebuild tool descriptions: compact index + dynamic skill descriptions.
		compactIdx := registry.CompactIndex()
		td := strings.Replace(prompts.Tools, "%TOOL_INDEX%", compactIdx, 1)
		if dynDescs := registry.DynamicSkillDescriptions(); dynDescs != "" {
			td += "\n" + dynDescs
		}
		if lb.ToolAgent != nil {
			lb.ToolAgent.ToolDescriptions = td
		}
		if eng.LLM.ToolAgent != nil {
			eng.LLM.ToolAgent.ToolDescriptions = td
		}

		// Update delegate_task's grammar and allowed-tools to include skills.
		if delegateConfig != nil {
			delegatable := []string{"search_email", "search_calendar", "search_memory", "save_memory", "search_web", "read_url", "run_command"}
			for _, s := range registry.Schemas() {
				if s.IsSkill {
					delegatable = append(delegatable, s.Name)
				}
			}
			dSchemas := registry.SchemasForTools(delegatable)
			dGrammar := grammar.BuildSubagentToolGrammar(dSchemas)
			delegateConfig.Update(delegatable, dGrammar)
		}
	}

	tools.AllowedInternalHosts = collectInternalHosts(cfg)
	skillDeps := tools.SkillDeps{Pool: db.Pool, Registry: registry, SC: svc.Subagent, DC: delegateConfig}
	registerSkillTools(registry, "skills", rebuildGrammar, skillDeps)
	registerPlanTools(registry, svc.DTC, svc.Subagent, delegateConfig, workTracker)
	rebuildGrammar() // include disk-loaded skills + plan tools in grammar

	// Wire hot-reload: skills sync on heartbeat tick, routines sync on
	// the independent routine scheduler tick. Shell config also syncs on heartbeat.
	skillMtimes := map[string]time.Time{}
	var routineMtime time.Time
	eng.Reloader = &hotReloader{
		syncSkills: func() {
			tools.SyncSkills(registry, "skills", rebuildGrammar, skillMtimes, skillDeps)
			if shellExec != nil {
				shellExec.SyncIfChanged()
			}
		},
		syncRoutines: func() {
			routines.SyncIfChanged(db.Pool, ".config/routines.toml", &routineMtime)
		},
	}

	// Wire reflection routing: inject reflection insights into conversation context.
	eng.ReflectionSink = &reflectionSinkAdapter{sm: svc.StateMgr}

	// Wire cognitive services (consolidation, synthesis, curiosity, objective inference).
	cog := &cognitiveAdapter{}
	cogAvailable := false
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		cog.consolidate = func(ctx context.Context) (int, error) {
			return pipelines.ConsolidateCore(ctx, pipelines.PipelineDeps{
				Pool: db.Pool, DTC: svc.DTC,
				EmbedEndpoint: cfg.EmbedURL, EmbedModel: cfg.EmbedModel,
				GrammarFn: svc.GrammarFunc,
			}, pipelines.ConsolidateOpts{
				SalienceThreshold: int(memory.SalienceHigh),
				MemoryLimit:       cfg.ConsolidationMemoryLimit,
			})
		}
		cogAvailable = true
	}
	if svc.SynthesizeFunc != nil {
		cog.synthesize = svc.SynthesizeFunc
		cogAvailable = true
	}
	if workTracker != nil && svc.DTC != nil && svc.Subagent != nil && delegateConfig != nil {
		cog.launchCuriosity = func(directive string, priority int, objectiveID int64) (int64, error) {
			return tools.LaunchBackgroundPlan(workTracker, tools.PlanExecDeps{SC: svc.Subagent, DTC: svc.DTC, DC: delegateConfig, Registry: registry}, directive, priority, objectiveID)
		}
		cogAvailable = true
	}
	if db.Pool != nil && svc.DTC != nil {
		capturedDTC := svc.DTC
		cog.inferObjectives = func(ctx context.Context) error {
			return engine.RunObjectiveInference(ctx, db.Pool, capturedDTC.Complete)
		}
		cogAvailable = true
	}
	if cogAvailable {
		eng.CogServices = cog
	}

	return wireResult{
		router:         router,
		rebuildGrammar: rebuildGrammar,
		skillMtimes:    skillMtimes,
		skillDeps:      skillDeps,
	}
}

// --- Startup Tasks ---

func runStartupTasks() {
	if db.Pool == nil {
		return
	}

	// One-time migration: move pending rows from legacy tasks table to work_items.
	migrateTasksTable()

	// Sync routines from TOML file → DB (TOML is source of truth).
	routines.SyncFromFile(db.Pool, ".config/routines.toml")

	// Cleanup and initial consolidation are now deferred to OnFirstTick in
	// the engine, so Qwen3.5-27B is free for interactive requests during startup.
}

// migrateTasksTable migrates pending rows from the old tasks table to
// work_items and drops the old table. Safe to call multiple times — no-ops
// if the tasks table doesn't exist.
func migrateTasksTable() {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.RoutineDB)
	defer cancel()

	var exists bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'tasks' AND table_schema = 'public'
		)`).Scan(&exists); err != nil || !exists {
		return
	}

	result, err := db.Pool.Exec(ctx,
		`INSERT INTO work_items (type, directive, status, due_at, recurrence, created_at)
		 SELECT 'scheduled', description, status, due_at, recurrence, created_at
		 FROM tasks WHERE status = 'pending'
		 ON CONFLICT DO NOTHING`)
	if err != nil {
		logger.Log.Warnf("[startup] failed to migrate tasks: %v", err)
		return
	}
	if result.RowsAffected() > 0 {
		logger.Log.Infof("[startup] migrated %d pending tasks to work_items", result.RowsAffected())
	}

	if _, err := db.Pool.Exec(ctx, `DROP TABLE IF EXISTS tasks`); err != nil {
		logger.Log.Warnf("[startup] failed to drop legacy tasks table: %v", err)
	} else {
		logger.Log.Info("[startup] dropped legacy tasks table")
	}
}
