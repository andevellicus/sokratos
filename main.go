package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"

	"sokratos/calendar"
	"sokratos/clients"
	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/gmail"
	"sokratos/googleauth"
	"sokratos/grammar"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/pipelines"
	"sokratos/prompts"
	"sokratos/routines"
	"sokratos/textutil"
	"sokratos/tools"
)

// --- Service Initialization ---

// serviceBundle holds all initialized services and shared closures.
type serviceBundle struct {
	Bot              *tgbotapi.BotAPI
	Updates          tgbotapi.UpdatesChannel
	DTC              *clients.DeepThinkerClient
	SynthesizeFunc   memory.SynthesizeFunc
	SubagentFunc     memory.SubagentFunc
	GrammarFunc      memory.GrammarSubagentFunc  // blocking + GBNF grammar (save_memory enrichment)
	BgGrammarFunc    memory.GrammarSubagentFunc  // non-blocking + GBNF grammar for entity extraction
	QueueFunc        memory.WorkQueueFunc        // background work queue (enrichment, distillation)
	DTCQueueFunc     memory.WorkQueueFunc        // DTC work queue for distillation
	Subagent         *clients.SubagentClient
	StateMgr         *engine.StateManager
	InterruptChan    chan struct{}
	TriageRetryQueue *pipelines.RetryQueue
}

func initServices(cfg *config.AppConfig) *serviceBundle {
	// Database.
	if cfg.DatabaseURL != "" {
		if err := db.Connect(context.Background(), cfg.DatabaseURL, db.DBPoolConfig{
			MaxConns:          cfg.DBMaxConns,
			MinConns:          cfg.DBMinConns,
			MaxConnLifetime:   cfg.DBMaxConnLifetime,
			MaxConnIdleTime:   cfg.DBMaxConnIdleTime,
			HealthCheckPeriod: cfg.DBHealthCheckPeriod,
		}); err != nil {
			logger.Log.Fatalf("Failed to connect to database: %v", err)
		}
	} else {
		logger.Log.Warn("DATABASE_URL is not set — running without database")
	}

	// Telegram bot.
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		logger.Log.Fatal(err)
	}
	logger.Log.Infof("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// Deep thinker client.
	var dtc *clients.DeepThinkerClient
	if cfg.DeepThinkerURL != "" {
		dtc = clients.NewDeepThinkerClient(cfg.DeepThinkerURL, cfg.DeepThinkerModel)
	}

	// SynthesizeFunc closure.
	var synthesizeFunc memory.SynthesizeFunc
	if dtc != nil {
		capturedDTC := dtc
		synthesizeFunc = func(ctx context.Context, systemPrompt, content string) (string, error) {
			return capturedDTC.Complete(ctx, systemPrompt, content, 2048)
		}
	}

	// DTC work queue closure.
	var dtcQueueFn memory.WorkQueueFunc
	if dtc != nil {
		capturedDTC := dtc
		dtcQueueFn = func(req memory.WorkRequest) {
			capturedDTC.QueueWork(req)
		}
	}

	// SubagentClient + SubagentFunc closure.
	// Use dedicated SubagentURL if set, otherwise fall back to the on-demand router.
	var subagent *clients.SubagentClient
	var subagentFunc memory.SubagentFunc
	var grammarFunc memory.GrammarSubagentFunc        // blocking + GBNF grammar (save_memory enrichment)
	var bgGrammarFunc memory.GrammarSubagentFunc     // non-blocking + GBNF grammar for entity extraction
	var queueFunc memory.WorkQueueFunc               // background work queue (enrichment, distillation)
	subagentURL := cfg.SubagentURL
	if subagentURL == "" {
		subagentURL = cfg.DeepThinkerURL
	}
	if subagentURL != "" && cfg.SubagentModel != "" {
		subagent = clients.NewSubagentClientNamed("subagent", subagentURL, cfg.SubagentModel, cfg.SubagentSlots)

		// Subagent handles all lightweight structured tasks — triage,
		// rewriting, re-ranking, tool calling. DTC is dedicated to heavy
		// reasoning (consolidation, synthesis, consulting). TryComplete
		// skips gracefully when slots are full.
		subagentFunc = func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
			return subagent.Complete(ctx, systemPrompt, userPrompt, 1024)
		}
		grammarFunc = func(ctx context.Context, systemPrompt, userPrompt, grammar string) (string, error) {
			return subagent.CompleteWithGrammar(ctx, systemPrompt, userPrompt, grammar, 1024)
		}
		bgGrammarFunc = func(ctx context.Context, systemPrompt, userPrompt, grammar string) (string, error) {
			return subagent.TryCompleteWithGrammar(ctx, systemPrompt, userPrompt, grammar, 1024)
		}
		queueFunc = func(req memory.WorkRequest) {
			subagent.QueueWork(req)
		}
		logger.Log.Info("[startup] subagent initialized (DTC dedicated to heavy reasoning)")
	}

	// OAuth via Telegram.
	telegramSend, telegramReceive := func(msg string) {
		for id := range cfg.AllowedIDs {
			m := tgbotapi.NewMessage(id, msg)
			bot.Send(m)
		}
	}, func() (string, error) {
		for update := range updates {
			if update.Message == nil || update.Message.Text == "" {
				continue
			}
			if len(cfg.AllowedIDs) > 0 {
				if _, ok := cfg.AllowedIDs[update.Message.From.ID]; !ok {
					continue
				}
			}
			return strings.TrimSpace(update.Message.Text), nil
		}
		return "", fmt.Errorf("update channel closed")
	}

	if err := gmail.Init(context.Background(), cfg.GmailCredsPath, cfg.GmailTokenPath, &googleauth.AuthIO{
		Send: telegramSend, Receive: telegramReceive,
	}); err != nil {
		logger.Log.Warnf("Gmail init failed: %v — Gmail features disabled", err)
	}

	if err := calendar.Init(context.Background(), cfg.GmailCredsPath, cfg.CalendarTokenPath, &googleauth.AuthIO{
		Send: telegramSend, Receive: telegramReceive,
	}); err != nil {
		logger.Log.Warnf("Calendar init failed: %v — Calendar features disabled", err)
	}

	stateMgr := engine.NewStateManager(db.Pool)
	stateMgr.LoadConversationSnapshot()

	interruptChan := make(chan struct{}, 1)

	if len(cfg.AllowedIDs) == 0 {
		logger.Log.Warn("ALLOWED_TELEGRAM_IDS is empty — bot will respond to everyone")
	}

	triageRetryQueue := pipelines.NewRetryQueue(pipelines.RetryQueueConfig{
		Name: "triage",
	})
	triageRetryQueue.Start()

	return &serviceBundle{
		Bot:              bot,
		Updates:          updates,
		DTC:              dtc,
		SynthesizeFunc:   synthesizeFunc,
		SubagentFunc:     subagentFunc,
		GrammarFunc:      grammarFunc,
		BgGrammarFunc:    bgGrammarFunc,
		QueueFunc:        queueFunc,
		DTCQueueFunc:   dtcQueueFn,
		Subagent:         subagent,
		StateMgr:         stateMgr,
		InterruptChan:    interruptChan,
		TriageRetryQueue: triageRetryQueue,
	}
}

// --- LLM Initialization ---

// llmBundle holds LLM-related initialization results.
type llmBundle struct {
	Client        *llm.Client
	ToolAgent     *llm.ToolAgentConfig
	TriageGrammar string
	TrimFn        func([]llm.Message) []llm.Message
}

func initLLM(cfg *config.AppConfig, registry *tools.Registry) *llmBundle {
	trimFn := func(msgs []llm.Message) []llm.Message {
		return engine.TrimMessages(msgs, 12)
	}

	llmClient := llm.NewClient(cfg.LLMURL)

	// Always create ToolAgentConfig for supervisor mode (parseToolIntent replaces
	// the former dedicated tool agent LLM). Start with the compact index; it will
	// be rebuilt with the full compact index + dynamic skill descriptions by
	// rebuildGrammar() after all tools are registered.
	compactIndex := registry.CompactIndex()
	toolDescs := strings.Replace(prompts.ToolsCompact, "%TOOL_INDEX%", compactIndex, 1)
	if dynDescs := registry.DynamicToolDescriptions(); dynDescs != "" {
		toolDescs += "\n" + dynDescs
	}
	toolAgentConfig := &llm.ToolAgentConfig{
		ToolDescriptions: toolDescs,
	}

	// Build triage grammar once for conversation triage via subagent.
	triageGrammar := grammar.BuildTriageGrammar()

	logger.Log.Info("Warming up LLM model...")
	warmupCtx, warmupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err := llmClient.Chat(warmupCtx, llm.ChatRequest{
		Model:    cfg.LLMModel,
		Messages: []llm.Message{{Role: "user", Content: "ping"}},
	})
	warmupCancel()
	if err != nil {
		logger.Log.Warnf("LLM warmup failed: %v (continuing anyway)", err)
	} else {
		logger.Log.Info("LLM model loaded and ready")
	}

	return &llmBundle{
		Client:        llmClient,
		ToolAgent:     toolAgentConfig,
		TriageGrammar: triageGrammar,
		TrimFn:        trimFn,
	}
}

// --- Engine Initialization ---

func initEngine(cfg *config.AppConfig, svc *serviceBundle, lb *llmBundle, registry *tools.Registry, fallbacks llm.FallbackMap) *engine.Engine {
	var mu sync.Mutex

	// Build ConsolidateFunc closure if dependencies are available.
	var consolidateFunc func(ctx context.Context) (int, error)
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		consolidateFunc = func(ctx context.Context) (int, error) {
			return pipelines.ConsolidateCore(ctx, db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel, pipelines.ConsolidateOpts{
				SalienceThreshold: int(memory.SalienceHigh),
				MemoryLimit:       cfg.ConsolidationMemoryLimit,
			}, svc.GrammarFunc)
		}
	}

	eng := &engine.Engine{
		LLM: engine.LLMConfig{
			Client:           lb.Client,
			Model:            cfg.LLMModel,
			ToolAgent:        lb.ToolAgent,
			Fallbacks:        fallbacks,
			MaxToolResultLen: cfg.MaxToolResultLen,
			MaxWebSources:    cfg.MaxWebSources,
		},
		Cognitive: engine.CognitiveConfig{
			BufferThreshold:           cfg.CognitiveBufferThreshold,
			LullDuration:              cfg.LullDuration,
			Ceiling:                   cfg.CognitiveCeiling,
			ConsolidateFunc:           consolidateFunc,
			ReflectionMemoryThreshold: cfg.ReflectionMemoryThreshold,
			ReflectionPrompt:          strings.TrimSpace(prompts.Reflection),
			SynthesizeFunc:            svc.SynthesizeFunc,
		},
		ToolExec:            registry.Execute,
		Mu:                  &mu,
		Interval:            cfg.HeartbeatInterval,
		RoutineInterval:     cfg.RoutineInterval,
		RoutineTimeout:      cfg.RoutineTimeout,
		SM:                  svc.StateMgr,
		DB:                  db.Pool,
		EmbedEndpoint:       cfg.EmbedURL,
		EmbedModel:          cfg.EmbedModel,
		MaxMessages:         40,
		MaintenanceInterval: cfg.MaintenanceInterval,
		MemoryStalenessDays: cfg.MemoryStalenessDays,
		SendFunc: func(text string) {
			for id := range cfg.AllowedIDs {
				msg := tgbotapi.NewMessage(id, mdToTelegramHTML(text))
				msg.ParseMode = tgbotapi.ModeHTML
				if _, err := svc.Bot.Send(msg); err != nil {
					msg.Text = text
					msg.ParseMode = ""
					if _, err := svc.Bot.Send(msg); err != nil {
						logger.Log.Errorf("Failed to send scheduled message to %d: %v", id, err)
					}
				}
			}
		},
		InterruptChan: svc.InterruptChan,
		Gatekeeper:    svc.Subagent,
		DTCQueueFunc:       svc.DTCQueueFunc,
		SubagentFunc:  svc.SubagentFunc,
		GrammarFunc:   svc.GrammarFunc,
		BgGrammarFunc: svc.BgGrammarFunc,
		QueueFunc:     svc.QueueFunc,
	}

	// Defer initial consolidation until after the first heartbeat tick so
	// Qwen3.5-27B is available for interactive requests during startup.
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		capturedDTC := svc.DTC
		eng.OnFirstTick = func() {
			pipelines.CleanupPreTriageMemories(db.Pool)
			pipelines.RunInitialConsolidation(db.Pool, capturedDTC, cfg.EmbedURL, cfg.EmbedModel, cfg.ConsolidationMemoryLimit, svc.GrammarFunc)
			eng.RefreshProfile()
			eng.RefreshPersonality()
		}
	}

	return eng
}

// --- Startup Tasks ---

func runStartupTasks() {
	if db.Pool == nil {
		return
	}

	// One-time migration: move pending rows from legacy tasks table to work_items.
	migrateTasksTable()

	// Sync routines from TOML file → DB (TOML is source of truth).
	routines.SyncFromFile(db.Pool, "routines.toml")

	// Cleanup and initial consolidation are now deferred to OnFirstTick in
	// the engine, so Qwen3.5-27B is free for interactive requests during startup.
}

// migrateTasksTable migrates pending rows from the old tasks table to
// work_items and drops the old table. Safe to call multiple times — no-ops
// if the tasks table doesn't exist.
func migrateTasksTable() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

// --- Main ---

func main() {
	if err := godotenv.Load(); err != nil {
		// Can't use logger yet, fall through.
	}

	if err := logger.Init("logs"); err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer logger.Close()

	cfg := config.Load()
	if cfg.TelegramToken == "" {
		logger.Log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	memory.MaxSupersededProfiles = cfg.MaxSupersededProfiles

	svc := initServices(cfg)
	defer db.Close()

	registry, emailTriageCfg, delegateConfig := registerTools(cfg, svc)

	var fallbacks llm.FallbackMap

	lb := initLLM(cfg, registry)

	// Set the triage grammar now that initLLM has built it. The NewSearchEmail
	// closure captures the pointer, so it sees the grammar when invoked.
	if emailTriageCfg != nil {
		emailTriageCfg.TriageGrammar = lb.TriageGrammar
	}

	// Send startup message to all allowed users.
	for id := range cfg.AllowedIDs {
		msg := tgbotapi.NewMessage(id, "Bot started and ready.")
		if _, err := svc.Bot.Send(msg); err != nil {
			logger.Log.Warnf("Failed to send startup message to %d: %v", id, err)
		}
	}

	eng := initEngine(cfg, svc, lb, registry, fallbacks)

	// Wire paradigm shift fast-path: after a paradigm shift is detected in
	// triage, run mini-consolidation then refresh the engine's profile state.
	if emailTriageCfg != nil {
		emailTriageCfg.ProfileRefreshFunc = func() {
			eng.RefreshProfile()
			eng.RefreshPersonality()
		}
	}

	// Work tracker: unified tracking for background plans, routines, and scheduled tasks.
	var workTracker *tools.WorkTracker
	if db.Pool != nil {
		workTracker = tools.NewWorkTracker(db.Pool, eng.SendFunc)
		workTracker.OnComplete = func(directive, status string) {
			summary := fmt.Sprintf("Background task %s: %s", status, textutil.Truncate(directive, 80))
			eng.RecordBackgroundCompletion("background_task", summary)
		}
		workTracker.CleanupOrphans()
		workTracker.CleanupOldTasks()
		eng.WorkMonitor = workTracker
	}

	// Register tools that need the engine for refresh callbacks.
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		registry.Register("consolidate_memory", pipelines.NewConsolidateMemory(
			db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel,
			cfg.ConsolidationMemoryLimit, svc.GrammarFunc, func() {
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
		td := strings.Replace(prompts.ToolsCompact, "%TOOL_INDEX%", compactIdx, 1)
		if dynDescs := registry.DynamicToolDescriptions(); dynDescs != "" {
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
			delegatable := []string{"search_email", "search_calendar", "search_memory", "save_memory", "search_web", "read_url"}
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
	registerSkillTools(registry, "skills", rebuildGrammar, db.Pool)
	registerPlanTools(registry, svc.DTC, svc.Subagent, delegateConfig, workTracker)
	rebuildGrammar() // include disk-loaded skills + plan tools in grammar

	// Wire hot-reload: skills sync on heartbeat tick, routines sync on
	// the independent routine scheduler tick.
	skillMtimes := map[string]time.Time{}
	eng.SyncFunc = func() {
		tools.SyncSkills(registry, "skills", rebuildGrammar, skillMtimes, db.Pool)
	}
	var routineMtime time.Time
	eng.RoutineSyncFunc = func() {
		routines.SyncIfChanged(db.Pool, "routines.toml", &routineMtime)
	}

	// Wire reflection routing: inject reflection insights into conversation context.
	eng.ReflectionNotifyFunc = func(summary string) {
		svc.StateMgr.AppendMessage(llm.Message{
			Role:    "user",
			Content: "[REFLECTION] A pattern was identified from recent memories:\n" + summary + "\nUse this if relevant to future interactions.",
		})
	}

	// Wire curiosity function for proactive research during cognitive lulls.
	if workTracker != nil && svc.DTC != nil && svc.Subagent != nil && delegateConfig != nil {
		eng.Cognitive.CuriosityFunc = func(directive string, priority int) (int64, error) {
			return tools.LaunchBackgroundPlan(workTracker, svc.DTC, svc.Subagent, delegateConfig, registry, directive, priority)
		}
	}

	// Wire goal inference for cognitive processing.
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		capturedDTC := svc.DTC
		eng.Cognitive.GoalInferenceFunc = func(ctx context.Context) error {
			return engine.RunGoalInference(ctx, db.Pool, capturedDTC.Complete, cfg.EmbedURL, cfg.EmbedModel, svc.GrammarFunc)
		}
	}

	runStartupTasks()

	// Start the engine after all fields, callbacks, and startup DB state are
	// fully wired. Any goroutine started earlier (SubagentClient workers, etc.)
	// does not access eng fields, so this is the correct synchronization point.
	go eng.Run()

	// Split the Telegram updates channel into messages and callback queries.
	// This allows confirmToolExec (used by both the message loop and the engine
	// heartbeat) to read callbacks without racing the main message loop.
	messageChan := make(chan *tgbotapi.Message, 50)
	callbackChan := make(chan *tgbotapi.CallbackQuery, 10)
	go func() {
		for update := range svc.Updates {
			if update.CallbackQuery != nil {
				callbackChan <- update.CallbackQuery
			} else if update.Message != nil {
				messageChan <- update.Message
			}
		}
		close(messageChan)
		close(callbackChan)
	}()

	confirmGate := map[string]bool{"send_email": true, "create_event": true, "create_skill": true}
	confirmExec := confirmToolExec(registry.Execute, svc.Bot, callbackChan, cfg.AllowedIDs, confirmGate, cfg.ConfirmationTimeout)

	// Wire the engine with the confirmation-gated executor too, so heartbeat-triggered
	// tools (e.g. send_email from a routine) also require user approval.
	eng.ToolExec = confirmExec

	mc := messageContext{
		cfg:            cfg,
		svc:            svc,
		eng:            eng,
		lb:             lb,
		registry:       registry,
		emailTriageCfg: emailTriageCfg,
		fallbacks:      fallbacks,
		confirmExec:    confirmExec,
		skillMtimes:    skillMtimes,
		rebuildGrammar: rebuildGrammar,
	}

	for msg := range messageChan {
		from := msg.From
		tag := senderTag(from)

		if len(cfg.AllowedIDs) > 0 {
			if _, ok := cfg.AllowedIDs[from.ID]; !ok {
				logger.Log.Warnf("Rejected message from %s: %q", tag, msg.Text)
				continue
			}
		}

		svc.StateMgr.TouchUserActivity()

		stateCtx := "**Current Time:** " + time.Now().Format(time.RFC3339) + "\n" + svc.StateMgr.GetState().ToMarkdown()
		if db.Pool != nil {
			stateCtx += engine.FetchPendingTasksMarkdown(context.Background(), db.Pool)
		}

		var userPrompt string
		var visionParts []llm.ContentPart
		var msgText string

		if photos := msg.Photo; len(photos) > 0 {
			photo := photos[len(photos)-1]
			imgData, mimeType, dlErr := downloadTelegramPhoto(svc.Bot, photo.FileID)
			if dlErr != nil {
				logger.Log.Errorf("Failed to download photo: %v", dlErr)
				continue
			}

			caption := msg.Caption
			if caption == "" {
				caption = "What's in this image?"
			}
			msgText = caption
			logger.Log.Infof("[%s] [photo] %s", tag, caption)

			userPrompt = caption + "\n\n[Current Agent State]\n" + stateCtx
			dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(imgData))
			visionParts = []llm.ContentPart{
				{Type: "text", Text: userPrompt},
				{Type: "image_url", ImageURL: &llm.ImageURL{URL: dataURI}},
			}
		} else if msg.Text != "" {
			msgText = msg.Text
			logger.Log.Infof("[%s] %s", tag, msgText)

			userPrompt = msgText + "\n\n[Current Agent State]\n" + stateCtx
		} else {
			continue
		}

		chatID := msg.Chat.ID

		// Direct-dispatch slash commands bypass the orchestrator entirely.
		switch strings.TrimSpace(msgText) {
		case "/reload":
			r := tgbotapi.NewMessage(chatID, handleReload(mc))
			r.ReplyToMessageID = msg.MessageID
			svc.Bot.Send(r)
		case "/bootstrap":
			r := tgbotapi.NewMessage(chatID, handleBootstrap(mc))
			r.ReplyToMessageID = msg.MessageID
			svc.Bot.Send(r)
		default:
			processMessage(mc, msg, chatID, msgText, userPrompt, visionParts)
		}
	}
}
