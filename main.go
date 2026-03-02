package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"

	"sokratos/calendar"
	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/gmail"
	"sokratos/googleauth"
	"sokratos/grammar"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/timeouts"
	"sokratos/tools"
)

// --- Fallback Map ---

// buildFallbackMap returns the deterministic fallback chains for tools that
// have known-good alternatives. This prevents the orchestrator from wasting
// rounds retrying tools that are likely to keep failing.
func buildFallbackMap() llm.FallbackMap {
	return llm.FallbackMap{
		"get-weather": {
			FallbackTool: "search_web",
			ArgsTransform: func(_ string, originalArgs json.RawMessage, _ string) json.RawMessage {
				var args struct {
					Location string `json:"location"`
				}
				if err := json.Unmarshal(originalArgs, &args); err != nil || args.Location == "" {
					args.Location = "current location"
				}
				b, _ := json.Marshal(map[string]string{"query": "weather " + args.Location})
				return b
			},
		},
		"get-news": {
			FallbackTool: "search_web",
			ArgsTransform: func(_ string, originalArgs json.RawMessage, _ string) json.RawMessage {
				var args struct {
					Topics string `json:"topics"`
					Query  string `json:"query"`
				}
				json.Unmarshal(originalArgs, &args)
				q := args.Topics
				if q == "" {
					q = args.Query
				}
				if q == "" {
					q = "latest news"
				}
				b, _ := json.Marshal(map[string]string{"query": q + " news"})
				return b
			},
		},
		"twitter-feed": {
			FallbackTool:   "search_web",
			TriggerPattern: regexp.MustCompile(`(?i)timeout|no results|error|failed|deadline`),
			ArgsTransform: func(_ string, originalArgs json.RawMessage, _ string) json.RawMessage {
				var args struct {
					Accounts string `json:"accounts"`
					Topics   string `json:"topics"`
				}
				json.Unmarshal(originalArgs, &args)
				q := args.Topics
				if q == "" {
					q = args.Accounts
				}
				if q == "" {
					q = "trending"
				}
				b, _ := json.Marshal(map[string]string{"query": "site:x.com " + q})
				return b
			},
		},
	}
}

// --- Service Initialization ---

// serviceBundle holds all initialized services and shared closures.
type serviceBundle struct {
	Bot              *tgbotapi.BotAPI
	Updates          tgbotapi.UpdatesChannel
	DTC              *tools.DeepThinkerClient
	SynthesizeFunc   memory.SynthesizeFunc
	SubagentFunc     memory.SubagentFunc
	BgSubagentFunc   memory.SubagentFunc        // non-blocking: skips when backends busy
	GrammarFunc      memory.GrammarSubagentFunc  // blocking + GBNF grammar (save_memory enrichment)
	BgGrammarFunc    memory.GrammarSubagentFunc  // non-blocking + GBNF grammar for entity extraction
	QueueFunc        memory.WorkQueueFunc        // background work queue (enrichment, distillation)
	Subagent         *tools.SubagentClient
	StateMgr         *engine.StateManager
	InterruptChan    chan struct{}
	TriageRetryQueue *tools.RetryQueue
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

	// SubagentClient + SubagentFunc closure.
	// Use dedicated SubagentURL if set, otherwise fall back to the on-demand router.
	var subagent *tools.SubagentClient
	var subagentFunc memory.SubagentFunc
	var bgSubagentFunc memory.SubagentFunc          // non-blocking variant for background work
	var grammarFunc memory.GrammarSubagentFunc        // blocking + GBNF grammar (save_memory enrichment)
	var bgGrammarFunc memory.GrammarSubagentFunc     // non-blocking + GBNF grammar for entity extraction
	var queueFunc memory.WorkQueueFunc               // background work queue (enrichment, distillation)
	subagentURL := cfg.SubagentURL
	if subagentURL == "" {
		subagentURL = cfg.DeepThinkerURL
	}
	if subagentURL != "" && cfg.SubagentModel != "" {
		subagent = tools.NewSubagentClientNamed("subagent-gemma3", subagentURL, cfg.SubagentModel, cfg.SubagentSlots)

		// Gemma3-only: Qwen3.5-27B is dedicated to DTC (consolidation,
		// synthesis, consulting). Gemma3-4B handles all subagent
		// work — triage, rewriting, re-ranking, tool calling. TryComplete
		// skips gracefully when slots are full.
		subagentFunc = func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
			return subagent.Complete(ctx, systemPrompt, userPrompt, 1024)
		}
		bgSubagentFunc = func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
			return subagent.TryComplete(ctx, systemPrompt, userPrompt, 1024)
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
		logger.Log.Info("[startup] subagent: gemma3-only (Qwen3.5-27B dedicated to DTC)")
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

	triageRetryQueue := tools.NewRetryQueue(tools.RetryQueueConfig{
		Name: "triage",
	})
	triageRetryQueue.Start()

	return &serviceBundle{
		Bot:              bot,
		Updates:          updates,
		DTC:              dtc,
		SynthesizeFunc:   synthesizeFunc,
		SubagentFunc:     subagentFunc,
		BgSubagentFunc:   bgSubagentFunc,
		GrammarFunc:      grammarFunc,
		BgGrammarFunc:    bgGrammarFunc,
		QueueFunc:        queueFunc,
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
			return tools.ConsolidateCore(ctx, db.Pool, svc.DTC, cfg.EmbedURL, cfg.EmbedModel, tools.ConsolidateOpts{
				SalienceThreshold: 8,
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
			tools.CleanupPreTriageMemories(db.Pool)
			tools.RunInitialConsolidation(db.Pool, capturedDTC, cfg.EmbedURL, cfg.EmbedModel, cfg.ConsolidationMemoryLimit, svc.GrammarFunc)
			eng.RefreshProfile()
			eng.RefreshPersonality()
		}
	}

	go eng.Run()
	return eng
}

// --- Startup Tasks ---

func runStartupTasks(cfg *config.AppConfig) {
	if db.Pool == nil {
		return
	}

	// Migrate personality traits from monolithic profile (one-time, synchronous).
	if cfg.EmbedURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), timeouts.PersonalityMigration)
		if err := tools.MigrateProfileToPersonality(ctx, db.Pool, cfg.EmbedURL, cfg.EmbedModel); err != nil {
			logger.Log.Warnf("[startup] personality migration failed: %v", err)
		}
		cancel()
	}

	// Sync routines from TOML file → DB (TOML is source of truth).
	SyncRoutinesFromFile("routines.toml")

	// Cleanup and initial consolidation are now deferred to OnFirstTick in
	// the engine, so Qwen3.5-27B is free for interactive requests during startup.
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

	svc := initServices(cfg)
	defer db.Close()

	registry, emailTriageCfg, delegateConfig := registerTools(cfg, svc)

	// Deterministic fallback chains: when a tool fails, automatically try a
	// known-good alternative instead of burning orchestrator rounds on retries.
	fallbacks := buildFallbackMap()

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

	// Background task runner (needs engine.SendFunc).
	var bgRunner *tools.BackgroundTaskRunner
	if db.Pool != nil {
		bgRunner = tools.NewBackgroundTaskRunner(db.Pool, eng.SendFunc)
		bgRunner.CleanupOrphans()
		bgRunner.CleanupOldTasks()
	}

	// Register tools that need the engine for refresh callbacks.
	if db.Pool != nil && svc.DTC != nil && cfg.EmbedURL != "" {
		registry.Register("consolidate_memory", tools.NewConsolidateMemory(
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
	registerPlanTools(registry, svc.DTC, svc.Subagent, delegateConfig, bgRunner)
	rebuildGrammar() // include disk-loaded skills + plan tools in grammar

	// Wire hot-reload: sync skills + routines from disk on each heartbeat.
	skillMtimes := map[string]time.Time{}
	var routineMtime time.Time
	eng.SyncFunc = func() {
		tools.SyncSkills(registry, "skills", rebuildGrammar, skillMtimes, db.Pool)
		syncRoutinesFile("routines.toml", &routineMtime)
	}

	// Wire reflection routing: inject reflection insights into conversation context.
	eng.ReflectionNotifyFunc = func(summary string) {
		svc.StateMgr.AppendMessage(llm.Message{
			Role:    "user",
			Content: "[REFLECTION] A pattern was identified from recent memories:\n" + summary + "\nUse this if relevant to future interactions.",
		})
	}

	// Wire curiosity function for proactive research during cognitive lulls.
	if bgRunner != nil && svc.DTC != nil && svc.Subagent != nil && delegateConfig != nil {
		eng.Cognitive.CuriosityFunc = func(directive string, priority int) (int64, error) {
			return tools.LaunchBackgroundPlan(bgRunner, svc.DTC, svc.Subagent, delegateConfig, registry, directive, priority)
		}
	}

	runStartupTasks(cfg)

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

		// Direct-dispatch slash commands — bypass the orchestrator entirely.
		if strings.TrimSpace(msgText) == "/reload" {
			// Force re-sync all TOML configs from disk → DB.
			added, updated, deleted := SyncRoutinesFromFile("routines.toml")
			skillsChanged := tools.SyncSkills(registry, "skills", rebuildGrammar, skillMtimes, db.Pool)
			var parts []string
			if len(added)+len(updated)+len(deleted) > 0 {
				parts = append(parts, fmt.Sprintf("Routines: +%d ~%d -%d", len(added), len(updated), len(deleted)))
			}
			if skillsChanged {
				parts = append(parts, "Skills: reloaded")
			}
			text := "Everything up to date."
			if len(parts) > 0 {
				text = "Reloaded: " + strings.Join(parts, ", ")
			}
			reply := tgbotapi.NewMessage(chatID, text)
			reply.ReplyToMessageID = msg.MessageID
			svc.Bot.Send(reply)
			continue
		}
		if strings.TrimSpace(msgText) == "/bootstrap" {
			if db.Pool == nil || svc.DTC == nil || cfg.EmbedURL == "" {
				reply := tgbotapi.NewMessage(chatID, "Bootstrap requires database, deep thinker, and embedding service.")
				reply.ReplyToMessageID = msg.MessageID
				svc.Bot.Send(reply)
				continue
			}
			bootstrapSend := func(text string) {
				for id := range cfg.AllowedIDs {
					m := tgbotapi.NewMessage(id, text)
					svc.Bot.Send(m)
				}
			}
			go tools.RunBootstrap(tools.BootstrapConfig{
				Pool:          db.Pool,
				DTC:           svc.DTC,
				EmbedEndpoint: cfg.EmbedURL,
				EmbedModel:    cfg.EmbedModel,
				AgentName:     cfg.AgentName,
				SendFunc:      bootstrapSend,
				OnProfile: func() {
					eng.RefreshProfile()
					eng.RefreshPersonality()
				},
			})
			reply := tgbotapi.NewMessage(chatID, "Profile generation started in the background. I'll notify you when it's ready.")
			reply.ReplyToMessageID = msg.MessageID
			svc.Bot.Send(reply)
			continue
		}

		typingCtx, typingCancel := context.WithCancel(context.Background())
		go sendTypingPeriodically(svc.Bot, chatID, typingCtx)

		// Phase 1: Snapshot history (StateManager has its own RWMutex).
		history := svc.StateMgr.ReadMessages()

		// Phase 2: Prefetch (network I/O — no engine state needed).
		var prefetchContent string
		var prefetchIDs []int64
		var prefetchSummaries string
		if db.Pool != nil && cfg.EmbedURL != "" && strings.TrimSpace(msgText) != "" {
			pfCtx, pfCancel := context.WithTimeout(context.Background(), tools.TimeoutPrefetch)
			if pf := subconsciousPrefetch(pfCtx, db.Pool, cfg.EmbedURL, cfg.EmbedModel, msgText, history); pf != nil {
				prefetchContent = pf.Summaries
				prefetchIDs = pf.IDs
				prefetchSummaries = pf.Summaries
			}
			pfCancel()
		}

		// Phase 2.5: Build temporal context (DB query — outside the lock).
		var temporalCtx string
		if db.Pool != nil {
			temporalCtx = engine.BuildTemporalContext(context.Background(), db.Pool)
		}

		// Phase 3: Lock for string snapshots + orchestrator serialization.
		eng.Mu.Lock()
		personalityContent := eng.PersonalityContent
		profileContent := eng.ProfileContent
		reply, msgs, err := llm.QueryOrchestrator(context.Background(), lb.Client, cfg.LLMModel, userPrompt, confirmExec, lb.TrimFn, &llm.QueryOrchestratorOpts{
			Parts:              visionParts,
			History:            history,
			PersonalityContent: personalityContent,
			ProfileContent:     profileContent,
			TemporalContext:    temporalCtx,
			PrefetchContent:    prefetchContent,
			MaxToolResultLen:   cfg.MaxToolResultLen,
			MaxWebSources:      cfg.MaxWebSources,
			ToolAgent:          lb.ToolAgent,
			Fallbacks:          fallbacks,
		})
		eng.Mu.Unlock()
		typingCancel()

		condensed := condenseToolResults(msgs)
		for _, m := range condensed {
			svc.StateMgr.AppendMessage(m)
		}
		if db.Pool != nil && cfg.EmbedURL != "" {
			engine.SlideAndArchiveContext(context.Background(), svc.StateMgr, eng.MaxMessages, engine.ArchiveDeps{
				DB: db.Pool, EmbedEndpoint: cfg.EmbedURL, EmbedModel: cfg.EmbedModel, SubagentFn: svc.SubagentFunc, GrammarFn: svc.GrammarFunc, BgGrammarFn: svc.BgGrammarFunc, QueueFn: svc.QueueFunc,
			})
		}

		if err == nil && db.Pool != nil && cfg.EmbedURL != "" && svc.DTC != nil {
			toolCtx, toolsUsed := summarizeToolContext(msgs)
			exchange := toolCtx + fmt.Sprintf("user: %s\nassistant: %s", msgText, reply)
			tools.TriageAndSaveConversationAsync(*emailTriageCfg, exchange, toolsUsed)
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

		// Format reply with entity-based markdown. `reply` already includes
		// accumulated intermediate text from tool-call rounds (prepended by
		// the supervisor). Thinking is logged in the supervisor, not shown in UI.
		fm := formatReply(reply)
		if _, err := sendFormatted(svc.Bot, chatID, msg.MessageID, fm); err != nil {
			logger.Log.Warnf("Entity send failed, falling back to plain text: %v", err)
			replyMsg := tgbotapi.NewMessage(chatID, reply)
			replyMsg.ReplyToMessageID = msg.MessageID
			if _, err := svc.Bot.Send(replyMsg); err != nil {
				logger.Log.Errorf("Error sending message: %v", err)
			}
		}
	}
}
