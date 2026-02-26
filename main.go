package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
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

	ToolAgentURL   string
	ToolAgentModel string

	VRAMPressureThreshold float64
	MaxWebSources         int
	MemorySearchLimit     int
	MaxToolResultLen      int

	ConsolidationInterval    time.Duration
	ConsolidationMemoryLimit int
	HeartbeatInterval        time.Duration
	EpisodeSynthesisInterval time.Duration

	MemoryStalenessDays       int
	ReflectionMemoryThreshold int

	DatabaseURL string

	GmailCredsPath    string
	GmailTokenPath    string
	CalendarTokenPath string

	EmailBackfillDays    int
	CalendarBackfillDays int
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

		ToolAgentURL:   os.Getenv("TOOL_AGENT_URL"),
		ToolAgentModel: os.Getenv("TOOL_AGENT_MODEL"),

		VRAMPressureThreshold: envFloat("VRAM_PRESSURE_THRESHOLD", 15.0),
		MaxWebSources:         envInt("MAX_WEB_SOURCES", 2),
		MemorySearchLimit:     envInt("MEMORY_SEARCH_LIMIT", 10),
		MaxToolResultLen:      envInt("MAX_TOOL_RESULT_LEN", 2000),

		ConsolidationInterval:    envDuration("CONSOLIDATION_INTERVAL", 1*time.Hour),
		ConsolidationMemoryLimit: envInt("CONSOLIDATION_MEMORY_LIMIT", 50),
		HeartbeatInterval:        envDuration("HEARTBEAT_INTERVAL", 5*time.Minute),
		EpisodeSynthesisInterval: envDuration("EPISODE_SYNTHESIS_INTERVAL", 6*time.Hour),

		MemoryStalenessDays:       envInt("MEMORY_STALENESS_DAYS", 90),
		ReflectionMemoryThreshold: envInt("REFLECTION_MEMORY_THRESHOLD", 50),

		DatabaseURL: os.Getenv("DATABASE_URL"),

		GmailCredsPath:    envString("GMAIL_CREDENTIALS_PATH", ".credentials/credentials.json"),
		GmailTokenPath:    envString("GMAIL_TOKEN_PATH", ".credentials/token.json"),
		CalendarTokenPath: envString("CALENDAR_TOKEN_PATH", ".credentials/calendar_token.json"),

		EmailBackfillDays:    envInt("EMAIL_BACKFILL_DAYS", 30),
		CalendarBackfillDays: envInt("CALENDAR_BACKFILL_DAYS", 90),
	}
}

// --- Service Initialization ---

// serviceBundle holds all initialized services and shared closures.
type serviceBundle struct {
	Bot            *tgbotapi.BotAPI
	Updates        tgbotapi.UpdatesChannel
	DTC            *tools.DeepThinkerClient
	SynthesizeFunc memory.SynthesizeFunc
	GraniteFunc    memory.GraniteFunc
	GraniteTriage  *tools.GraniteTriageConfig
	StateMgr       *engine.StateManager
	VRAMAuditor    *engine.VRAMAuditor
	InterruptChan  chan struct{}
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
			return capturedDTC.Complete(ctx, systemPrompt, content, 2048)
		}
	}

	// GraniteFunc closure.
	var graniteFunc memory.GraniteFunc
	if cfg.ToolAgentURL != "" && cfg.ToolAgentModel != "" {
		gClient := llm.NewClient(cfg.ToolAgentURL)
		gClient.EnableThinking = false
		gModel := cfg.ToolAgentModel
		graniteFunc = func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
			result, err := gClient.Chat(ctx, llm.ChatRequest{
				Model: gModel,
				Messages: []llm.Message{
					{Role: "system", Content: systemPrompt},
					{Role: "user", Content: userPrompt},
				},
			})
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(result.Message.Content), nil
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

	if err := gmail.Init(context.Background(), cfg.GmailCredsPath, cfg.GmailTokenPath, &gmail.AuthIO{
		Send: telegramSend, Receive: telegramReceive,
	}); err != nil {
		logger.Log.Warnf("Gmail init failed: %v — Gmail features disabled", err)
	}

	if err := calendar.Init(context.Background(), cfg.GmailCredsPath, cfg.CalendarTokenPath, &calendar.AuthIO{
		Send: telegramSend, Receive: telegramReceive,
	}); err != nil {
		logger.Log.Warnf("Calendar init failed: %v — Calendar features disabled", err)
	}

	stateMgr := engine.NewStateManager(db.Pool)

	interruptChan := make(chan struct{}, 1)

	// VRAM auditor.
	vramAuditor := engine.NewVRAMAuditor(cfg.VRAMPressureThreshold)
	if cfg.DeepThinkerURL != "" {
		vramAuditor.Register("deep_thinker", cfg.DeepThinkerURL,
			cfg.DeepThinkerModel, 19200)
		if dtc != nil {
			dtc.OnAccess = func() { vramAuditor.UpdateAccessTime("deep_thinker") }
		}
	}
	if cfg.Text2SQLURL != "" {
		vramAuditor.Register("text2sql", cfg.Text2SQLURL,
			"Arctic-Text2SQL-R1-7B.Q8_0", 7800)
	}
	go vramAuditor.Run(context.Background())

	if len(cfg.AllowedIDs) == 0 {
		logger.Log.Warn("ALLOWED_TELEGRAM_IDS is empty — bot will respond to everyone")
	}

	return &serviceBundle{
		Bot:            bot,
		Updates:        updates,
		DTC:            dtc,
		SynthesizeFunc: synthesizeFunc,
		GraniteFunc:    graniteFunc,
		StateMgr:       stateMgr,
		VRAMAuditor:    vramAuditor,
		InterruptChan:  interruptChan,
	}
}

// --- Tool Registration (grouped by domain) ---

func registerCoreTools(registry *tools.Registry, stateMgr *engine.StateManager, cfg *appConfig) {
	registry.Register("get_server_time", tools.GetServerTime, tools.ToolSchema{
		Name: "get_server_time",
	})
	if cfg.SearxngURL != "" {
		registry.Register("search_web", tools.NewSearchWeb(cfg.SearxngURL), tools.ToolSchema{
			Name:   "search_web",
			Params: []tools.ParamSchema{{Name: "query", Type: "string", Required: true}},
		})
	}
	registry.Register("read_url", tools.NewReadURL(cfg.EmbedURL, cfg.EmbedModel), tools.ToolSchema{
		Name: "read_url",
		Params: []tools.ParamSchema{
			{Name: "url", Type: "string", Required: true},
			{Name: "query", Type: "string", Required: false},
		},
	})
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

func registerDBTools(registry *tools.Registry, pool *pgxpool.Pool, interruptChan chan struct{}, text2sqlURL string, vramAuditor *engine.VRAMAuditor) {
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
	registry.Register("manage_directives", tools.NewManageDirectives(pool), tools.ToolSchema{
		Name: "manage_directives",
		Params: []tools.ParamSchema{
			{Name: "action", Type: "string", Required: true},
			{Name: "name", Type: "string", Required: true},
			{Name: "interval", Type: "string", Required: false},
			{Name: "instruction", Type: "string", Required: false},
		},
	})
	if text2sqlURL != "" {
		registry.Register("ask_database", tools.NewAskDatabase(pool, text2sqlURL,
			func() { vramAuditor.UpdateAccessTime("text2sql") }), tools.ToolSchema{
			Name:   "ask_database",
			Params: []tools.ParamSchema{{Name: "natural_language_query", Type: "string", Required: true}},
		})
	}
}

func registerGmailTools(registry *tools.Registry, pool *pgxpool.Pool, cfg *appConfig, dtc *tools.DeepThinkerClient, graniteFunc memory.GraniteFunc) {
	if pool != nil && cfg.EmbedURL != "" {
		registry.Register("search_inbox", tools.NewSearchInbox(pool, cfg.EmbedURL, cfg.EmbedModel), tools.ToolSchema{
			Name: "search_inbox",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: false},
				{Name: "from", Type: "string", Required: false},
				{Name: "to", Type: "string", Required: false},
				{Name: "subject", Type: "string", Required: false},
				{Name: "max_results", Type: "number", Required: false},
			},
		})
	}
	if gmail.Service != nil && dtc != nil && pool != nil && cfg.EmbedURL != "" {
		registry.Register("check_email", tools.NewCheckEmail(gmail.Service, pool, cfg.EmbedURL, cfg.EmbedModel, dtc, graniteFunc), tools.ToolSchema{
			Name: "check_email",
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

func registerCalendarTools(registry *tools.Registry, pool *pgxpool.Pool, cfg *appConfig, dtc *tools.DeepThinkerClient, graniteFunc memory.GraniteFunc) {
	if pool != nil && cfg.EmbedURL != "" {
		registry.Register("search_calendar", tools.NewSearchCalendar(pool, cfg.EmbedURL, cfg.EmbedModel), tools.ToolSchema{
			Name: "search_calendar",
			Params: []tools.ParamSchema{
				{Name: "query", Type: "string", Required: false},
				{Name: "title", Type: "string", Required: false},
				{Name: "attendee", Type: "string", Required: false},
				{Name: "max_results", Type: "number", Required: false},
			},
		})
	}
	if calendar.Service != nil && dtc != nil && pool != nil && cfg.EmbedURL != "" {
		registry.Register("check_calendar", tools.NewCheckCalendar(calendar.Service, pool, cfg.EmbedURL, cfg.EmbedModel, dtc, graniteFunc), tools.ToolSchema{
			Name:   "check_calendar",
			Params: []tools.ParamSchema{{Name: "days", Type: "number", Required: false}},
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

func registerAITools(registry *tools.Registry, dtc *tools.DeepThinkerClient) {
	if dtc == nil {
		return
	}
	registry.Register("consult_deep_thinker", func(ctx context.Context, args json.RawMessage) (string, error) {
		return tools.ConsultDeepThinker(ctx, args, dtc)
	}, tools.ToolSchema{
		Name: "consult_deep_thinker",
		Params: []tools.ParamSchema{
			{Name: "problem_statement", Type: "string", Required: true},
			{Name: "max_tokens", Type: "number", Required: false},
		},
	})
}

func registerTools(cfg *appConfig, svc *serviceBundle) *tools.Registry {
	registry := tools.NewRegistry()

	registerCoreTools(registry, svc.StateMgr, cfg)
	registerDBTools(registry, db.Pool, svc.InterruptChan, cfg.Text2SQLURL, svc.VRAMAuditor)
	registerAITools(registry, svc.DTC)

	if db.Pool != nil && cfg.EmbedURL != "" {
		tools.RegisterMemoryTools(registry, db.Pool, cfg.EmbedURL, cfg.EmbedModel, cfg.ToolAgentURL, cfg.ToolAgentModel, cfg.MemorySearchLimit)
	}
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		registry.Register("consolidate_memory", tools.NewConsolidateMemory(db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel, cfg.ConsolidationMemoryLimit), tools.ToolSchema{
			Name: "consolidate_memory",
		})
	}

	registerGmailTools(registry, db.Pool, cfg, svc.DTC, svc.GraniteFunc)
	registerCalendarTools(registry, db.Pool, cfg, svc.DTC, svc.GraniteFunc)

	return registry
}

// --- LLM Initialization ---

// llmBundle holds LLM-related initialization results.
type llmBundle struct {
	Client        *llm.Client
	ToolAgent     *llm.ToolAgentConfig
	GraniteTriage *tools.GraniteTriageConfig
	ToolGrammar   string
	TrimFn        func([]llm.Message) []llm.Message
}

func initLLM(cfg *appConfig, registry *tools.Registry) *llmBundle {
	toolGrammar := grammar.BuildToolGrammar(registry.Schemas())
	trimFn := func(msgs []llm.Message) []llm.Message {
		return engine.TrimMessages(msgs, 12)
	}

	llmClient := llm.NewClient(cfg.LLMURL)

	var toolAgentConfig *llm.ToolAgentConfig
	if cfg.ToolAgentURL != "" && cfg.ToolAgentModel != "" {
		toolAgentGrammar := grammar.BuildToolGrammar(registry.ToolSchemas())
		toolAgentClient := llm.NewClient(cfg.ToolAgentURL)
		toolAgentClient.EnableThinking = false
		toolAgentConfig = &llm.ToolAgentConfig{
			Client:  toolAgentClient,
			Model:   cfg.ToolAgentModel,
			Grammar: toolAgentGrammar,
		}
		logger.Log.Infof("Tool agent enabled: %s (%s)", cfg.ToolAgentURL, cfg.ToolAgentModel)
	}

	var graniteTriage *tools.GraniteTriageConfig
	if cfg.ToolAgentURL != "" && cfg.ToolAgentModel != "" {
		triageClient := llm.NewClient(cfg.ToolAgentURL)
		triageClient.EnableThinking = false
		graniteTriage = &tools.GraniteTriageConfig{
			Client:  triageClient,
			Model:   cfg.ToolAgentModel,
			Grammar: grammar.BuildTriageGrammar(),
		}
		logger.Log.Info("Granite conversation triage enabled")
	}

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
		GraniteTriage: graniteTriage,
		ToolGrammar:   toolGrammar,
		TrimFn:        trimFn,
	}
}

// --- Engine Initialization ---

func initEngine(cfg *appConfig, svc *serviceBundle, lb *llmBundle, registry *tools.Registry) *engine.Engine {
	var mu sync.Mutex

	eng := &engine.Engine{
		Client:                lb.Client,
		Model:                 cfg.LLMModel,
		ToolExec:              registry.Execute,
		Mu:                    &mu,
		Interval:              cfg.HeartbeatInterval,
		ConsolidationInterval: cfg.ConsolidationInterval,
		SM:                    svc.StateMgr,
		DB:                    db.Pool,
		EmbedEndpoint:         cfg.EmbedURL,
		EmbedModel:            cfg.EmbedModel,
		MaxMessages:           40,
		Grammar:               lb.ToolGrammar,
		ToolAgent:             lb.ToolAgent,
		ProfileContent:        "",
		MaxToolResultLen:      cfg.MaxToolResultLen,
		MaxWebSources:         cfg.MaxWebSources,
		MemoryStalenessDays:   cfg.MemoryStalenessDays,
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
		InterruptChan:             svc.InterruptChan,
		EpisodeSynthesisInterval:  cfg.EpisodeSynthesisInterval,
		ReflectionMemoryThreshold: cfg.ReflectionMemoryThreshold,
		ReflectionPrompt:          strings.TrimSpace(prompts.Reflection),
		SynthesizeFunc:            svc.SynthesizeFunc,
	}
	go eng.Run()
	return eng
}

// --- Startup Tasks ---

func runStartupTasks(cfg *appConfig, svc *serviceBundle) {
	if db.Pool == nil || svc.DTC == nil {
		return
	}
	go func() {
		tools.CleanupPreTriageMemories(db.Pool)

		if gmail.Service != nil && cfg.EmbedURL != "" {
			tools.RunEmailBackfill(tools.BackfillConfig{
				BackfillBase: tools.BackfillBase{
					Pool:          db.Pool,
					EmbedEndpoint: cfg.EmbedURL,
					EmbedModel:    cfg.EmbedModel,
					DTC:           svc.DTC,
					BackfillDays:  cfg.EmailBackfillDays,
				},
				GmailService: gmail.Service,
			})
		}
		if calendar.Service != nil && cfg.EmbedURL != "" {
			tools.RunCalendarBackfill(tools.CalendarBackfillConfig{
				BackfillBase: tools.BackfillBase{
					Pool:          db.Pool,
					EmbedEndpoint: cfg.EmbedURL,
					EmbedModel:    cfg.EmbedModel,
					DTC:           svc.DTC,
					BackfillDays:  cfg.CalendarBackfillDays,
				},
				CalendarService: calendar.Service,
			})
		}
		tools.RunInitialConsolidation(db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel, cfg.ConsolidationMemoryLimit)
	}()
}

// --- Telegram Helpers ---

func parseAllowedIDs(raw string) map[int64]struct{} {
	allowed := make(map[int64]struct{})
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		allowed[id] = struct{}{}
	}
	return allowed
}

func senderTag(from *tgbotapi.User) string {
	if from.UserName != "" {
		return fmt.Sprintf("@%s", from.UserName)
	}
	name := strings.TrimSpace(from.FirstName + " " + from.LastName)
	if name != "" {
		return fmt.Sprintf("%s (id:%d)", name, from.ID)
	}
	return fmt.Sprintf("id:%d", from.ID)
}

// sendTypingPeriodically sends a "typing..." chat action immediately and then
// every 5 seconds until ctx is cancelled.
func sendTypingPeriodically(bot *tgbotapi.BotAPI, chatID int64, ctx context.Context) {
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	bot.Send(action)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bot.Send(action)
		}
	}
}

// downloadTelegramPhoto fetches a photo from Telegram's servers and returns
// the raw bytes along with the detected MIME type.
func downloadTelegramPhoto(bot *tgbotapi.BotAPI, fileID string) ([]byte, string, error) {
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return nil, "", fmt.Errorf("get file: %w", err)
	}

	resp, err := http.Get(file.Link(bot.Token))
	if err != nil {
		return nil, "", fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read file: %w", err)
	}

	mime := "image/jpeg"
	switch strings.ToLower(path.Ext(file.FilePath)) {
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	case ".bmp":
		mime = "image/bmp"
	}

	return data, mime, nil
}

// formatConfirmation builds a human-readable confirmation prompt for a tool call.
func formatConfirmation(name string, args json.RawMessage) string {
	switch name {
	case "send_email":
		var a struct {
			To      string `json:"to"`
			Subject string `json:"subject"`
		}
		_ = json.Unmarshal(args, &a)
		return fmt.Sprintf("⚠ Confirm: Send email to %s with subject %q? (yes/no)", a.To, a.Subject)
	case "create_event":
		var a struct {
			Title string `json:"title"`
			Start string `json:"start"`
		}
		_ = json.Unmarshal(args, &a)
		return fmt.Sprintf("⚠ Confirm: Create event %q at %s? (yes/no)", a.Title, a.Start)
	default:
		return fmt.Sprintf("⚠ Confirm: Execute %s? (yes/no)", name)
	}
}

// confirmToolExec wraps a tool executor with Telegram confirmation for
// externally-visible actions.
func confirmToolExec(
	base func(context.Context, json.RawMessage) (string, error),
	bot *tgbotapi.BotAPI,
	updates tgbotapi.UpdatesChannel,
	allowedIDs map[int64]struct{},
	confirmTools map[string]bool,
) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &call); err == nil && confirmTools[call.Name] {
			desc := formatConfirmation(call.Name, call.Arguments)

			for id := range allowedIDs {
				msg := tgbotapi.NewMessage(id, desc)
				bot.Send(msg)
			}

			timer := time.NewTimer(2 * time.Minute)
			defer timer.Stop()
			for {
				select {
				case update := <-updates:
					if update.Message == nil || update.Message.Text == "" {
						continue
					}
					if len(allowedIDs) > 0 {
						if _, ok := allowedIDs[update.Message.From.ID]; !ok {
							continue
						}
					}
					answer := strings.ToLower(strings.TrimSpace(update.Message.Text))
					if strings.HasPrefix(answer, "y") {
						return base(ctx, raw)
					}
					return "Action cancelled by user.", nil
				case <-timer.C:
					return "Action cancelled — confirmation timed out.", nil
				}
			}
		}
		return base(ctx, raw)
	}
}

// --- Memory Prefetch ---

// prefetchResult holds the subconscious prefetch output: a system message
// with retrieved context, plus the memory IDs for usefulness feedback.
type prefetchResult struct {
	Message *llm.Message
	IDs     []int64
}

// subconsciousPrefetch embeds the user's message and retrieves semantically
// similar memories as background context.
func subconsciousPrefetch(ctx context.Context, pool *pgxpool.Pool, embedURL, embedModel, msgText string, recentMessages []llm.Message) *prefetchResult {
	// Build trajectory string from recent user messages for contextual vector recall.
	var trajectoryParts []string
	count := 0
	for i := len(recentMessages) - 1; i >= 0 && count < 3; i-- {
		m := recentMessages[i]
		if m.Role != "user" {
			continue
		}
		text := m.Content
		if idx := strings.Index(text, "\n\n[Current Agent State]"); idx > 0 {
			text = text[:idx]
		}
		if len(text) > 200 {
			text = text[:200]
		}
		text = strings.TrimSpace(text)
		if text != "" {
			trajectoryParts = append(trajectoryParts, text)
			count++
		}
	}
	for i, j := 0, len(trajectoryParts)-1; i < j; i, j = i+1, j-1 {
		trajectoryParts[i], trajectoryParts[j] = trajectoryParts[j], trajectoryParts[i]
	}
	trajectoryParts = append(trajectoryParts, msgText)
	trajectoryStr := strings.Join(trajectoryParts, " | ")

	pf := memory.Prefetch(ctx, pool, embedURL, embedModel, trajectoryStr, msgText, 3)
	if pf == nil {
		return nil
	}
	return &prefetchResult{
		Message: &llm.Message{Role: "system", Content: pf.Content},
		IDs:     pf.IDs,
	}
}

// evaluateMemoryUsefulness calls Granite to determine whether prefetched
// memories contributed to the assistant's response.
func evaluateMemoryUsefulness(pool *pgxpool.Pool, graniteURL, graniteModel string, memoryIDs []int64, userMsg, assistantReply string) {
	ctx, cancel := context.WithTimeout(context.Background(), tools.TimeoutUsefulnessEval)
	defer cancel()

	client := llm.NewClient(graniteURL)
	client.EnableThinking = false

	prompt := fmt.Sprintf("User message: %s\n\nAssistant response: %s\n\n"+
		"Were the retrieved memory contexts useful in generating this response? "+
		"Answer with exactly YES or NO.", userMsg, assistantReply)

	result, err := client.Chat(ctx, llm.ChatRequest{
		Model: graniteModel,
		Messages: []llm.Message{
			{Role: "system", Content: "You evaluate whether retrieved memory context was useful for generating an assistant response. Answer with exactly YES or NO. Nothing else."},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		logger.Log.Warnf("[usefulness] Granite evaluation failed: %v", err)
		return
	}

	answer := strings.TrimSpace(strings.ToUpper(result.Message.Content))
	wasUseful := strings.HasPrefix(answer, "YES")
	memory.RecordMemoryUsefulness(ctx, pool, memoryIDs, wasUseful)
	logger.Log.Debugf("[usefulness] memories %v: useful=%v (raw=%q)", memoryIDs, wasUseful, answer)
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
	svc.GraniteTriage = lb.GraniteTriage

	// Send startup message to all allowed users.
	for id := range cfg.AllowedIDs {
		msg := tgbotapi.NewMessage(id, "Bot started and ready.")
		if _, err := svc.Bot.Send(msg); err != nil {
			logger.Log.Warnf("Failed to send startup message to %d: %v", id, err)
		}
	}

	eng := initEngine(cfg, svc, lb, registry)
	runStartupTasks(cfg, svc)

	confirmExec := confirmToolExec(registry.Execute, svc.Bot, svc.Updates, cfg.AllowedIDs,
		map[string]bool{"send_email": true, "create_event": true})

	for update := range svc.Updates {
		if update.Message == nil {
			continue
		}

		from := update.Message.From
		tag := senderTag(from)

		if len(cfg.AllowedIDs) > 0 {
			if _, ok := cfg.AllowedIDs[from.ID]; !ok {
				logger.Log.Warnf("Rejected message from %s: %q", tag, update.Message.Text)
				continue
			}
		}

		stateCtx := "**Current Time:** " + time.Now().Format(time.RFC3339) + "\n" + svc.StateMgr.GetState().ToMarkdown()
		if db.Pool != nil {
			stateCtx += engine.FetchPendingTasksMarkdown(context.Background(), db.Pool)
		}

		var userPrompt string
		var visionParts []llm.ContentPart
		var msgText string

		if photos := update.Message.Photo; len(photos) > 0 {
			photo := photos[len(photos)-1]
			imgData, mimeType, dlErr := downloadTelegramPhoto(svc.Bot, photo.FileID)
			if dlErr != nil {
				logger.Log.Errorf("Failed to download photo: %v", dlErr)
				continue
			}

			caption := update.Message.Caption
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
		} else if update.Message.Text != "" {
			msgText = update.Message.Text
			logger.Log.Infof("[%s] %s", tag, msgText)
			userPrompt = msgText + "\n\n[Current Agent State]\n" + stateCtx
		} else {
			continue
		}

		chatID := update.Message.Chat.ID
		typingCtx, typingCancel := context.WithCancel(context.Background())
		go sendTypingPeriodically(svc.Bot, chatID, typingCtx)

		eng.Mu.Lock()
		history := svc.StateMgr.ReadMessages()

		inferHistory := history
		var prefetchIDs []int64
		if db.Pool != nil && cfg.EmbedURL != "" && strings.TrimSpace(msgText) != "" {
			pfCtx, pfCancel := context.WithTimeout(context.Background(), tools.TimeoutPrefetch)
			if pf := subconsciousPrefetch(pfCtx, db.Pool, cfg.EmbedURL, cfg.EmbedModel, msgText, history); pf != nil {
				inferHistory = make([]llm.Message, len(history), len(history)+1)
				copy(inferHistory, history)
				inferHistory = append(inferHistory, *pf.Message)
				prefetchIDs = pf.IDs
			}
			pfCancel()
		}

		profileContent := eng.ProfileContent
		reply, msgs, err := llm.QueryOrchestrator(context.Background(), lb.Client, cfg.LLMModel, userPrompt, confirmExec, lb.TrimFn, &llm.QueryOrchestratorOpts{
			Parts:            visionParts,
			History:          inferHistory,
			Grammar:          lb.ToolGrammar,
			ProfileContent:   profileContent,
			MaxToolResultLen: cfg.MaxToolResultLen,
			MaxWebSources:    cfg.MaxWebSources,
			ToolAgent:        lb.ToolAgent,
		})
		eng.Mu.Unlock()
		typingCancel()

		for _, m := range msgs {
			svc.StateMgr.AppendMessage(m)
		}
		if db.Pool != nil && cfg.EmbedURL != "" {
			engine.SlideAndArchiveContext(context.Background(), svc.StateMgr, eng.MaxMessages, db.Pool, cfg.EmbedURL, cfg.EmbedModel)
		}

		if err == nil && db.Pool != nil && cfg.EmbedURL != "" && svc.DTC != nil {
			exchange := fmt.Sprintf("user: %s\nassistant: %s", msgText, reply)
			tools.TriageAndSaveConversationAsync(db.Pool, cfg.EmbedURL, cfg.EmbedModel, svc.DTC, svc.GraniteFunc, lb.GraniteTriage, exchange)
		}

		if err == nil && len(prefetchIDs) > 0 && db.Pool != nil && cfg.ToolAgentURL != "" && cfg.ToolAgentModel != "" {
			capturedIDs := prefetchIDs
			capturedReply := reply
			capturedMsgText := msgText
			go evaluateMemoryUsefulness(db.Pool, cfg.ToolAgentURL, cfg.ToolAgentModel, capturedIDs, capturedMsgText, capturedReply)
		}

		if err != nil {
			logger.Log.Errorf("LLM error: %v", err)
			reply = "Sorry, something went wrong processing your message."
		}

		msg := tgbotapi.NewMessage(update.Message.Chat.ID, mdToTelegramHTML(reply))
		msg.ReplyToMessageID = update.Message.MessageID
		msg.ParseMode = tgbotapi.ModeHTML

		if _, err := svc.Bot.Send(msg); err != nil {
			logger.Log.Warnf("HTML send failed, falling back to plain text: %v", err)
			msg.Text = reply
			msg.ParseMode = ""
			if _, err := svc.Bot.Send(msg); err != nil {
				logger.Log.Errorf("Error sending message: %v", err)
			}
		}
	}
}
