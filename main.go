package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"sokratos/calendar"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/gmail"
	"sokratos/googleauth"
	"sokratos/grammar"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/tools"
)

// --- appConfig ---

// appConfig holds all parsed environment configuration.
type appConfig struct {
	TelegramToken string
	AllowedIDs    map[int64]struct{}

	LLMURL   string
	LLMModel string

	SearxngURL string

	EmbedURL   string
	EmbedModel string

	DeepThinkerURL   string
	DeepThinkerModel string

	Text2SQLURL string

	SubagentURL   string
	SubagentModel string
	SubagentSlots int

	BackgroundSubagentSlots int // subagent-z1 semaphore (default 1; queues behind DTC on Z1's single slot)

	MaxWebSources int
	MemorySearchLimit     int
	MaxToolResultLen      int

	ConsolidationMemoryLimit int
	HeartbeatInterval        time.Duration

	CognitiveBufferThreshold int
	LullDuration             time.Duration
	CognitiveCeiling         time.Duration

	MemoryStalenessDays       int
	ReflectionMemoryThreshold int

	DatabaseURL string

	GmailCredsPath    string
	GmailTokenPath    string
	CalendarTokenPath string
}

// loadConfig parses all environment variables into an appConfig struct.
func loadConfig() *appConfig {
	return &appConfig{
		TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		AllowedIDs:    parseAllowedIDs(os.Getenv("ALLOWED_TELEGRAM_IDS")),

		LLMURL:   envString("LLM_URL", "http://localhost:11434"),
		LLMModel: os.Getenv("LLM_MODEL"),

		SearxngURL: os.Getenv("SEARXNG_URL"),

		EmbedURL:   os.Getenv("EMBEDDING_URL"),
		EmbedModel: os.Getenv("EMBEDDING_MODEL"),

		DeepThinkerURL:   os.Getenv("DEEP_THINKER_URL"),
		DeepThinkerModel: os.Getenv("DEEP_THINKER_MODEL"),

		Text2SQLURL: os.Getenv("TEXT2SQL_URL"),

		SubagentURL:   os.Getenv("SUBAGENT_URL"),
		SubagentModel: os.Getenv("SUBAGENT_MODEL"),
		SubagentSlots: envInt("SUBAGENT_SLOTS", 2),

		BackgroundSubagentSlots: envInt("BACKGROUND_SUBAGENT_SLOTS", 1),

		MaxWebSources: envInt("MAX_WEB_SOURCES", 2),
		MemorySearchLimit:     envInt("MEMORY_SEARCH_LIMIT", 10),
		MaxToolResultLen:      envInt("MAX_TOOL_RESULT_LEN", 2000),

		ConsolidationMemoryLimit: envInt("CONSOLIDATION_MEMORY_LIMIT", 50),
		HeartbeatInterval:        envDuration("HEARTBEAT_INTERVAL", 5*time.Minute),

		CognitiveBufferThreshold: envInt("COGNITIVE_BUFFER_THRESHOLD", 20),
		LullDuration:             envDuration("LULL_DURATION", 20*time.Minute),
		CognitiveCeiling:         envDuration("COGNITIVE_CEILING", 4*time.Hour),

		MemoryStalenessDays:       envInt("MEMORY_STALENESS_DAYS", 90),
		ReflectionMemoryThreshold: envInt("REFLECTION_MEMORY_THRESHOLD", 50),

		DatabaseURL: os.Getenv("DATABASE_URL"),

		GmailCredsPath:    envString("GMAIL_CREDENTIALS_PATH", ".credentials/credentials.json"),
		GmailTokenPath:    envString("GMAIL_TOKEN_PATH", ".credentials/token.json"),
		CalendarTokenPath: envString("CALENDAR_TOKEN_PATH", ".credentials/calendar_token.json"),
	}
}

// --- Service Initialization ---

// serviceBundle holds all initialized services and shared closures.
type serviceBundle struct {
	Bot            *tgbotapi.BotAPI
	Updates        tgbotapi.UpdatesChannel
	DTC            *tools.DeepThinkerClient
	SynthesizeFunc memory.SynthesizeFunc
	SubagentFunc   memory.SubagentFunc
	Subagent       *tools.SubagentClient
	StateMgr      *engine.StateManager
	InterruptChan chan struct{}
}

func initServices(cfg *appConfig) *serviceBundle {
	// Database.
	if cfg.DatabaseURL != "" {
		if err := db.Connect(context.Background(), cfg.DatabaseURL); err != nil {
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
	var dtc *tools.DeepThinkerClient
	if cfg.DeepThinkerURL != "" {
		dtc = tools.NewDeepThinkerClient(cfg.DeepThinkerURL, cfg.DeepThinkerModel)
	}

	// SynthesizeFunc closure.
	var synthesizeFunc memory.SynthesizeFunc
	if dtc != nil {
		capturedDTC := dtc
		synthesizeFunc = func(ctx context.Context, systemPrompt, content string) (string, error) {
			return capturedDTC.CompleteNoThink(ctx, systemPrompt, content, 2048)
		}
	}

	// SubagentClient + SubagentFunc closure.
	// Use dedicated SubagentURL if set, otherwise fall back to the on-demand router.
	var subagent *tools.SubagentClient
	var subagentFunc memory.SubagentFunc
	subagentURL := cfg.SubagentURL
	if subagentURL == "" {
		subagentURL = cfg.DeepThinkerURL
	}
	if subagentURL != "" && cfg.SubagentModel != "" {
		subagent = tools.NewSubagentClientNamed("subagent-flash", subagentURL, cfg.SubagentModel, cfg.SubagentSlots)

		// Create a SubagentPool that distributes background work across both
		// Flash (fast, 2 slots) and Z1 (overflow, queues behind DTC on its
		// single 32K slot). Prevents contention when triage, contradiction,
		// entity extraction, and quality scoring fire simultaneously.
		if cfg.DeepThinkerURL != "" && cfg.DeepThinkerModel != "" {
			z1Subagent := tools.NewSubagentClientNamed("subagent-z1", cfg.DeepThinkerURL, cfg.DeepThinkerModel, cfg.BackgroundSubagentSlots)
			pool := tools.NewSubagentPool(subagent, z1Subagent)
			subagentFunc = pool.Func()
			logger.Log.Info("[startup] subagent pool: flash + z1 backends")
		} else {
			subagentFunc = func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
				return subagent.Complete(ctx, systemPrompt, userPrompt, 1024)
			}
		}
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

	interruptChan := make(chan struct{}, 1)

	if len(cfg.AllowedIDs) == 0 {
		logger.Log.Warn("ALLOWED_TELEGRAM_IDS is empty — bot will respond to everyone")
	}

	return &serviceBundle{
		Bot:            bot,
		Updates:        updates,
		DTC:            dtc,
		SynthesizeFunc: synthesizeFunc,
		SubagentFunc:   subagentFunc,
		Subagent:      subagent,
		StateMgr:      stateMgr,
		InterruptChan: interruptChan,
	}
}

// --- Tool Registration (grouped by domain) ---

func registerCoreTools(registry *tools.Registry, stateMgr *engine.StateManager) {
	registry.Register("update_state", tools.NewUpdateState(stateMgr), tools.ToolSchema{
		Name: "update_state",
		Params: []tools.ParamSchema{
			{Name: "status", Type: "string", Required: true},
			{Name: "task", Type: "string", Required: true},
		},
	})
	registry.Register("set_preference", tools.NewSetPreference(stateMgr), tools.ToolSchema{
		Name: "set_preference",
		Params: []tools.ParamSchema{
			{Name: "key", Type: "string", Required: true},
			{Name: "value", Type: "string", Required: true},
		},
	})
}

func registerDBTools(registry *tools.Registry, pool *pgxpool.Pool, interruptChan chan struct{}, text2sqlURL string) {
	if pool == nil {
		return
	}
	registry.Register("add_task", tools.NewAddTask(pool, interruptChan), tools.ToolSchema{
		Name: "add_task",
		Params: []tools.ParamSchema{
			{Name: "task", Type: "string", Required: true},
			{Name: "due_at", Type: "string", Required: false},
			{Name: "recur", Type: "string", Required: false},
		},
	})
	registry.Register("complete_task", tools.NewCompleteTask(pool, interruptChan), tools.ToolSchema{
		Name:   "complete_task",
		Params: []tools.ParamSchema{{Name: "task_id", Type: "number", Required: false}},
	})
	registry.Register("manage_routines", tools.NewManageRoutines(pool), tools.ToolSchema{
		Name: "manage_routines",
		Params: []tools.ParamSchema{
			{Name: "action", Type: "string", Required: true},
			{Name: "name", Type: "string", Required: true},
			{Name: "interval", Type: "string", Required: false},
			{Name: "instruction", Type: "string", Required: false},
		},
	})
	if text2sqlURL != "" {
		registry.Register("ask_database", tools.NewAskDatabase(pool, text2sqlURL), tools.ToolSchema{
			Name:   "ask_database",
			Params: []tools.ParamSchema{{Name: "natural_language_query", Type: "string", Required: true}},
		})
	}
}

func registerGmailTools(registry *tools.Registry, pool *pgxpool.Pool) {
	if gmail.Service != nil {
		registry.Register("search_email", tools.NewSearchEmail(gmail.Service, pool), tools.ToolSchema{
			Name: "search_email",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: false},
				{Name: "time_min", Type: "string", Required: false},
				{Name: "time_max", Type: "string", Required: false},
				{Name: "max_results", Type: "number", Required: false},
			},
		})
	}
	if gmail.Service != nil {
		registry.Register("send_email", tools.NewSendEmail(gmail.Service), tools.ToolSchema{
			Name: "send_email",
			Params: []tools.ParamSchema{
				{Name: "to", Type: "string", Required: true},
				{Name: "subject", Type: "string", Required: true},
				{Name: "body", Type: "string", Required: true},
			},
		})
	}
}

func registerCalendarTools(registry *tools.Registry) {
	if calendar.Service != nil {
		registry.Register("search_calendar", tools.NewSearchCalendar(calendar.Service), tools.ToolSchema{
			Name: "search_calendar",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: false},
				{Name: "time_min", Type: "string", Required: false},
				{Name: "time_max", Type: "string", Required: false},
				{Name: "max_results", Type: "number", Required: false},
			},
		})
	}
	if calendar.Service != nil {
		registry.Register("create_event", tools.NewCreateEvent(calendar.Service), tools.ToolSchema{
			Name: "create_event",
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
}

func registerAITools(registry *tools.Registry, dtc *tools.DeepThinkerClient, subagent *tools.SubagentClient) {
	if dtc != nil {
		registry.Register("consult_deep_thinker", tools.NewConsultDeepThinker(dtc), tools.ToolSchema{
			Name: "consult_deep_thinker",
			Params: []tools.ParamSchema{
				{Name: "problem_statement", Type: "string", Required: true},
				{Name: "max_tokens", Type: "number", Required: false},
			},
		})
	}
	if subagent != nil {
		registry.Register("dispatch_subagent", tools.NewDispatchSubagent(subagent), tools.ToolSchema{
			Name: "dispatch_subagent",
			Params: []tools.ParamSchema{
				{Name: "task_routine", Type: "string", Required: true},
				{Name: "raw_data", Type: "string", Required: true},
			},
		})
	}
}

func registerSkillTools(registry *tools.Registry, skillsDir string, rebuildGrammar tools.GrammarRebuildFunc) {
	skills, err := tools.LoadSkills(skillsDir)
	if err != nil {
		logger.Log.Warnf("Failed to load skills: %v", err)
	}
	for _, skill := range skills {
		tools.RegisterSkill(registry, skill)
	}
	registry.Register("create_skill", tools.NewCreateSkill(registry, skillsDir, rebuildGrammar), tools.ToolSchema{
		Name: "create_skill",
		Params: []tools.ParamSchema{
			{Name: "name", Type: "string", Required: true},
			{Name: "description", Type: "string", Required: true},
			{Name: "params", Type: "string", Required: false},
			{Name: "code", Type: "string", Required: true},
			{Name: "test_args", Type: "string", Required: true},
		},
	})
	registry.Register("manage_skills", tools.NewManageSkills(registry, skillsDir, rebuildGrammar), tools.ToolSchema{
		Name: "manage_skills",
		Params: []tools.ParamSchema{
			{Name: "action", Type: "string", Required: true},
			{Name: "name", Type: "string", Required: false},
		},
	})
}

// collectInternalHosts extracts host:port pairs from configured service URLs
// for the skill HTTP bridge allowlist.
func collectInternalHosts(cfg *appConfig) []string {
	var hosts []string
	for _, raw := range []string{cfg.SearxngURL, cfg.EmbedURL} {
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

func registerTools(cfg *appConfig, svc *serviceBundle) *tools.Registry {
	registry := tools.NewRegistry()

	registerCoreTools(registry, svc.StateMgr)
	registerDBTools(registry, db.Pool, svc.InterruptChan, cfg.Text2SQLURL)
	registerAITools(registry, svc.DTC, svc.Subagent)

	if db.Pool != nil && cfg.EmbedURL != "" {
		registry.Register("search_memory", tools.NewSearchMemory(db.Pool, cfg.EmbedURL, cfg.EmbedModel, svc.Subagent, cfg.MemorySearchLimit), tools.ToolSchema{
			Name: "search_memory",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: true},
				{Name: "tags", Type: "array", Required: false},
				{Name: "start_date", Type: "string", Required: false},
				{Name: "end_date", Type: "string", Required: false},
				{Name: "memory_type", Type: "string", Required: false},
			},
		})
		registry.Register("save_memory", tools.NewSaveMemory(db.Pool, cfg.EmbedURL, cfg.EmbedModel, svc.SubagentFunc), tools.ToolSchema{
			Name: "save_memory",
			Params: []tools.ParamSchema{
				{Name: "summary", Type: "string", Required: true},
				{Name: "tags", Type: "array", Required: false},
				{Name: "category", Type: "string", Required: false},
				{Name: "salience_score", Type: "number", Required: false},
				{Name: "memory_type", Type: "string", Required: false},
			},
		})
	}
	if db.Pool != nil && cfg.EmbedURL != "" {
		registry.Register("forget_topic", tools.NewForgetTopic(db.Pool, cfg.EmbedURL, cfg.EmbedModel), tools.ToolSchema{
			Name: "forget_topic",
			Params: []tools.ParamSchema{
				{Name: "topic", Type: "string", Required: true},
				{Name: "confirm", Type: "boolean", Required: false},
			},
		})
	}
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		registry.Register("bootstrap_profile", tools.NewBootstrapProfile(db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel, envString("AGENT_NAME", "Sokratos")), tools.ToolSchema{
			Name: "bootstrap_profile",
		})
	}

	registerGmailTools(registry, db.Pool)
	registerCalendarTools(registry)

	return registry
}

// --- LLM Initialization ---

// llmBundle holds LLM-related initialization results.
type llmBundle struct {
	Client        *llm.Client
	ToolAgent     *llm.ToolAgentConfig
	ToolGrammar   string
	TriageGrammar string
	TrimFn        func([]llm.Message) []llm.Message
}

func initLLM(cfg *appConfig, registry *tools.Registry) *llmBundle {
	toolGrammar := grammar.BuildToolGrammar(registry.Schemas())
	trimFn := func(msgs []llm.Message) []llm.Message {
		return engine.TrimMessages(msgs, 12)
	}

	llmClient := llm.NewClient(cfg.LLMURL)

	// Always create ToolAgentConfig for supervisor mode (parseToolIntent replaces
	// the former dedicated tool agent LLM).
	toolAgentConfig := &llm.ToolAgentConfig{
		ToolDescriptions: prompts.Tools,
	}

	// Build triage grammar once for conversation triage via subagent.
	triageGrammar := grammar.BuildTriageGrammar()

	logger.Log.Info("Warming up LLM model...")
	_, err := llmClient.Chat(context.Background(), llm.ChatRequest{
		Model:    cfg.LLMModel,
		Messages: []llm.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		logger.Log.Warnf("LLM warmup failed: %v", err)
	} else {
		logger.Log.Info("LLM model loaded and ready")
	}

	return &llmBundle{
		Client:        llmClient,
		ToolAgent:     toolAgentConfig,
		ToolGrammar:   toolGrammar,
		TriageGrammar: triageGrammar,
		TrimFn:        trimFn,
	}
}

// --- Engine Initialization ---

func initEngine(cfg *appConfig, svc *serviceBundle, lb *llmBundle, registry *tools.Registry) *engine.Engine {
	var mu sync.Mutex

	// Build ConsolidateFunc closure if dependencies are available.
	var consolidateFunc func(ctx context.Context) (int, error)
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		consolidateFunc = func(ctx context.Context) (int, error) {
			return tools.ConsolidateCore(ctx, db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel, tools.ConsolidateOpts{
				SalienceThreshold: 8,
				MemoryLimit:       cfg.ConsolidationMemoryLimit,
			})
		}
	}

	eng := &engine.Engine{
		LLM: engine.LLMConfig{
			Client:           lb.Client,
			Model:            cfg.LLMModel,
			Grammar:          lb.ToolGrammar,
			ToolAgent:        lb.ToolAgent,
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
		SM:                  svc.StateMgr,
		DB:                  db.Pool,
		EmbedEndpoint:       cfg.EmbedURL,
		EmbedModel:          cfg.EmbedModel,
		MaxMessages:         40,
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
		SubagentFunc:  svc.SubagentFunc,
	}
	go eng.Run()
	return eng
}

// --- Startup Tasks ---

func runStartupTasks(cfg *appConfig, svc *serviceBundle) {
	if db.Pool == nil {
		return
	}

	// Migrate personality traits from monolithic profile (one-time, synchronous).
	if cfg.EmbedURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := tools.MigrateProfileToPersonality(ctx, db.Pool, cfg.EmbedURL, cfg.EmbedModel); err != nil {
			logger.Log.Warnf("[startup] personality migration failed: %v", err)
		}
		cancel()
	}

	if svc.DTC == nil {
		return
	}
	go func() {
		tools.CleanupPreTriageMemories(db.Pool)
		tools.RunInitialConsolidation(db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel, cfg.ConsolidationMemoryLimit)
	}()
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

	cfg := loadConfig()
	if cfg.TelegramToken == "" {
		logger.Log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	svc := initServices(cfg)
	defer db.Close()

	registry := registerTools(cfg, svc)
	lb := initLLM(cfg, registry)

	// Send startup message to all allowed users.
	for id := range cfg.AllowedIDs {
		msg := tgbotapi.NewMessage(id, "Bot started and ready.")
		if _, err := svc.Bot.Send(msg); err != nil {
			logger.Log.Warnf("Failed to send startup message to %d: %v", id, err)
		}
	}

	eng := initEngine(cfg, svc, lb, registry)

	// Register tools that need the engine for refresh callbacks.
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		registry.Register("consolidate_memory", tools.NewConsolidateMemory(
			db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel,
			cfg.ConsolidationMemoryLimit, func() {
				eng.RefreshProfile()
				eng.RefreshPersonality()
			},
		), tools.ToolSchema{
			Name: "consolidate_memory",
		})
	}
	if db.Pool != nil {
		registry.Register("manage_personality", tools.NewManagePersonality(db.Pool, func() {
			eng.RefreshPersonality()
		}), tools.ToolSchema{
			Name: "manage_personality",
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
		newGrammar := grammar.BuildToolGrammar(registry.Schemas())
		lb.ToolGrammar = newGrammar
		eng.LLM.Grammar = newGrammar

		// Rebuild tool descriptions: static core tools + dynamic skill descriptions.
		dynDescs := registry.DynamicToolDescriptions()
		toolDescs := prompts.Tools
		if dynDescs != "" {
			toolDescs += "\n" + dynDescs
		}
		if lb.ToolAgent != nil {
			lb.ToolAgent.ToolDescriptions = toolDescs
		}
		if eng.LLM.ToolAgent != nil {
			eng.LLM.ToolAgent.ToolDescriptions = toolDescs
		}
	}

	tools.AllowedInternalHosts = collectInternalHosts(cfg)
	registerSkillTools(registry, "skills", rebuildGrammar)
	rebuildGrammar() // include disk-loaded skills in grammar

	runStartupTasks(cfg, svc)

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

	confirmGate := map[string]bool{"send_email": true, "create_event": true, "create_skill": true, "bootstrap_profile": true}
	confirmExec := confirmToolExec(registry.Execute, svc.Bot, callbackChan, cfg.AllowedIDs, confirmGate)

	// Wire the engine with the confirmation-gated executor too, so heartbeat-triggered
	// tools (e.g. send_email from a routine) also require user approval.
	eng.ToolExec = confirmExec

	triageCfg := tools.TriageConfig{
		Pool:          db.Pool,
		EmbedEndpoint: cfg.EmbedURL,
		EmbedModel:    cfg.EmbedModel,
		DTC:           svc.DTC,
		SubagentFn:    svc.SubagentFunc,
		Subagent:      svc.Subagent,
		TriageGrammar: lb.TriageGrammar,
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

			// Translate Telegram slash commands into explicit tool instructions.
			effectivePrompt := msgText
			switch strings.TrimSpace(msgText) {
			case "/bootstrap":
				effectivePrompt = "Run the bootstrap_profile tool now to generate my initial identity profile."
			}

			userPrompt = effectivePrompt + "\n\n[Current Agent State]\n" + stateCtx
		} else {
			continue
		}

		chatID := msg.Chat.ID
		typingCtx, typingCancel := context.WithCancel(context.Background())
		go sendTypingPeriodically(svc.Bot, chatID, typingCtx)

		eng.Mu.Lock()
		history := svc.StateMgr.ReadMessages()

		inferHistory := history
		var prefetchIDs []int64
		var prefetchSummaries string
		if db.Pool != nil && cfg.EmbedURL != "" && strings.TrimSpace(msgText) != "" {
			pfCtx, pfCancel := context.WithTimeout(context.Background(), tools.TimeoutPrefetch)
			if pf := subconsciousPrefetch(pfCtx, db.Pool, cfg.EmbedURL, cfg.EmbedModel, msgText, history); pf != nil {
				inferHistory = make([]llm.Message, len(history), len(history)+1)
				copy(inferHistory, history)
				inferHistory = append(inferHistory, *pf.Message)
				prefetchIDs = pf.IDs
				prefetchSummaries = pf.Summaries
			}
			pfCancel()
		}

		personalityContent := eng.PersonalityContent
		profileContent := eng.ProfileContent
		reply, msgs, err := llm.QueryOrchestrator(context.Background(), lb.Client, cfg.LLMModel, userPrompt, confirmExec, lb.TrimFn, &llm.QueryOrchestratorOpts{
			Parts:              visionParts,
			History:            inferHistory,
			Grammar:            lb.ToolGrammar,
			PersonalityContent: personalityContent,
			ProfileContent:     profileContent,
			MaxToolResultLen:   cfg.MaxToolResultLen,
			MaxWebSources:      cfg.MaxWebSources,
			ToolAgent:          lb.ToolAgent,
		})
		eng.Mu.Unlock()
		typingCancel()

		for _, m := range msgs {
			svc.StateMgr.AppendMessage(m)
		}
		if db.Pool != nil && cfg.EmbedURL != "" {
			engine.SlideAndArchiveContext(context.Background(), svc.StateMgr, eng.MaxMessages, engine.ArchiveDeps{
				DB: db.Pool, EmbedEndpoint: cfg.EmbedURL, EmbedModel: cfg.EmbedModel, SubagentFn: svc.SubagentFunc,
			})
		}

		if err == nil && db.Pool != nil && cfg.EmbedURL != "" && svc.DTC != nil {
			exchange := fmt.Sprintf("user: %s\nassistant: %s", msgText, reply)
			toolsUsed := false
			for _, m := range msgs {
				if strings.HasPrefix(m.Content, "Tool result: ") {
					toolsUsed = true
					break
				}
			}
			tools.TriageAndSaveConversationAsync(triageCfg, exchange, toolsUsed)
		}

		if err == nil && len(prefetchIDs) > 0 && db.Pool != nil && svc.Subagent != nil {
			capturedIDs := prefetchIDs
			capturedReply := reply
			capturedMsgText := msgText
			capturedSubagent := svc.Subagent
			capturedSummaries := prefetchSummaries
			go evaluateMemoryUsefulnessViaSubagent(db.Pool, capturedSubagent, capturedIDs, capturedMsgText, capturedReply, capturedSummaries)
		}

		if err != nil {
			logger.Log.Errorf("LLM error: %v", err)
			reply = "Sorry, something went wrong processing your message."
		}

		// Don't send orchestrator control tags to the user.
		if strings.Contains(reply, "<NO_ACTION_REQUIRED>") {
			continue
		}

		replyMsg := tgbotapi.NewMessage(chatID, mdToTelegramHTML(reply))
		replyMsg.ReplyToMessageID = msg.MessageID
		replyMsg.ParseMode = tgbotapi.ModeHTML

		if _, err := svc.Bot.Send(replyMsg); err != nil {
			logger.Log.Warnf("HTML send failed, falling back to plain text: %v", err)
			replyMsg.Text = reply
			replyMsg.ParseMode = ""
			if _, err := svc.Bot.Send(replyMsg); err != nil {
				logger.Log.Errorf("Error sending message: %v", err)
			}
		}
	}
}
