package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"

	"sokratos/clients"
	"sokratos/config"
	"sokratos/db"
	"sokratos/engine"
	"sokratos/google"
	"sokratos/grammar"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/metrics"
	"sokratos/pipelines"
	"sokratos/platform"
	"sokratos/platform/telegram"
	"sokratos/prompts"
	"sokratos/timeouts"
	"sokratos/tokens"
	"sokratos/tools"
)

// --- Service Initialization ---

// serviceBundle holds all initialized services and shared closures.
type serviceBundle struct {
	Platform         platform.Platform
	DTC              *clients.DeepThinkerClient
	SubagentFunc     memory.SubagentFunc
	GrammarFunc      memory.GrammarSubagentFunc // blocking + GBNF grammar (save_memory enrichment)
	BgGrammarFunc    memory.GrammarSubagentFunc // non-blocking + GBNF grammar for entity extraction
	QueueFunc        memory.WorkQueueFunc       // background work queue (enrichment, distillation)
	DTCQueueFunc     memory.WorkQueueFunc       // DTC work queue for distillation
	Subagent         *clients.SubagentClient
	StateMgr         *engine.StateManager
	InterruptChan    chan struct{}
	Metrics          *metrics.Collector
	TriageRetryQueue *pipelines.RetryQueue
	AuthErrorOnce    *sync.Once // fires once per token expiry
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

	// Telegram platform.
	plat, err := telegram.New(cfg.TelegramToken, cfg.AllowedIDs)
	if err != nil {
		logger.Log.Fatal(err)
	}

	// Deep thinker client.
	var dtc *clients.DeepThinkerClient
	if cfg.DeepThinkerURL != "" {
		dtc = clients.NewDeepThinkerClient(cfg.DeepThinkerURL, cfg.DeepThinkerModel)
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
	var grammarFunc memory.GrammarSubagentFunc   // blocking + GBNF grammar (save_memory enrichment)
	var bgGrammarFunc memory.GrammarSubagentFunc // non-blocking + GBNF grammar for entity extraction
	var queueFunc memory.WorkQueueFunc           // background work queue (enrichment, distillation)
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
			return subagent.Complete(ctx, systemPrompt, userPrompt, tokens.SubagentGeneral)
		}
		grammarFunc = func(ctx context.Context, systemPrompt, userPrompt, grammar string) (string, error) {
			return subagent.CompleteWithGrammar(ctx, systemPrompt, userPrompt, grammar, tokens.SubagentGeneral)
		}
		bgGrammarFunc = func(ctx context.Context, systemPrompt, userPrompt, grammar string) (string, error) {
			return subagent.TryCompleteWithGrammar(ctx, systemPrompt, userPrompt, grammar, tokens.SubagentGeneral)
		}
		queueFunc = func(req memory.WorkRequest) {
			subagent.QueueWork(req)
		}
		logger.Log.Info("[startup] subagent initialized (DTC dedicated to heavy reasoning)")
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
		Platform:         plat,
		DTC:              dtc,
		SubagentFunc:     subagentFunc,
		GrammarFunc:      grammarFunc,
		BgGrammarFunc:    bgGrammarFunc,
		QueueFunc:        queueFunc,
		DTCQueueFunc:     dtcQueueFn,
		Subagent:         subagent,
		StateMgr:         stateMgr,
		Metrics:          metrics.New(db.Pool),
		InterruptChan:    interruptChan,
		TriageRetryQueue: triageRetryQueue,
		AuthErrorOnce:    &sync.Once{},
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
		return engine.TrimMessages(msgs, engine.DefaultMaxTail)
	}

	llmClient := llm.NewClient(cfg.LLMURL)

	// Always create ToolAgentConfig for supervisor mode (parseToolIntent replaces
	// the former dedicated tool agent LLM). Start with the compact index; it will
	// be rebuilt with the full compact index + dynamic skill descriptions by
	// rebuildGrammar() after all tools are registered.
	toolDescs := strings.Replace(prompts.Tools, "%TOOL_INDEX%", registry.FullToolIndex(), 1)
	toolAgentConfig := &llm.ToolAgentConfig{
		ToolDescriptions: toolDescs,
		Parser:           llm.SupervisorParser{IsKnownTool: registry.Has},
	}

	// Build triage grammar once for conversation triage via subagent.
	triageGrammar := grammar.BuildTriageGrammar()

	logger.Log.Info("Warming up LLM model...")
	warmupCtx, warmupCancel := context.WithTimeout(context.Background(), timeouts.LLMWarmup)
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

	// Two-model mode: when BRAIN_URL is set, the 9B serves as the primary
	// orchestrator (fast, user-facing) and the 122B Brain serves as the deep
	// thinker (consolidation, triage, synthesis). The Brain is also the
	// orchestrator fallback when all 9B slots are busy.
	if cfg.BrainURL != "" {
		cfg.LLMURL = cfg.SubagentURL
		cfg.LLMModel = cfg.SubagentModel
		cfg.DeepThinkerURL = cfg.BrainURL
		cfg.DeepThinkerModel = cfg.BrainModel
		logger.Log.Infof("[startup] two-model mode: Orchestrator=%s/%s, Brain=%s/%s",
			cfg.SubagentURL, cfg.SubagentModel, cfg.BrainURL, cfg.BrainModel)
	}

	memory.MaxSupersededProfiles = cfg.MaxSupersededProfiles

	// Ensure workspace directory exists.
	if err := os.MkdirAll(cfg.WorkspaceDir, 0755); err != nil {
		logger.Log.Fatalf("Failed to create workspace directory %q: %v", cfg.WorkspaceDir, err)
	}
	if absWs, err := filepath.Abs(cfg.WorkspaceDir); err == nil {
		logger.Log.Infof("[startup] workspace directory: %s", absWs)
	}

	svc := initServices(cfg)
	defer db.Close()
	defer svc.Metrics.Close()

	// Non-blocking Google auth: load existing tokens silently.
	if err := google.InitGmailFromToken(context.Background(), cfg.GmailCredsPath, cfg.GoogleTokenPath); err != nil {
		logger.Log.Warnf("Gmail init failed: %v — Gmail features disabled", err)
	}
	if err := google.InitCalendarFromToken(context.Background(), cfg.GmailCredsPath, cfg.GoogleTokenPath); err != nil {
		logger.Log.Warnf("Calendar init failed: %v — Calendar features disabled", err)
	}

	tb := registerTools(cfg, svc)
	registry := tb.Registry

	// Wire proactive auth-error notification: when any Google API call fails
	// due to an expired/revoked token, notify the user once via platform.
	registry.OnAuthError = func(toolName string) {
		svc.AuthErrorOnce.Do(func() {
			logger.Log.Warnf("[auth] detected auth expiry via %s — notifying user", toolName)
			os.Remove(cfg.GoogleTokenPath)
			google.GmailService = nil
			google.CalendarService = nil
			svc.Platform.Broadcast(context.Background(), "⚠️ Google authorization expired. Use /google to re-authenticate.")
		})
	}

	lb := initLLM(cfg, registry)

	// Set the triage grammar now that initLLM has built it. The NewSearchEmail
	// closure captures the pointer, so it sees the grammar when invoked.
	if tb.EmailTriageCfg != nil {
		tb.EmailTriageCfg.TriageGrammar = lb.TriageGrammar
	}

	// Register platform slash commands.
	if err := svc.Platform.RegisterCommands([]platform.Command{
		{Name: "reload", Description: "Reload skills and routines from disk"},
		{Name: "bootstrap", Description: "Generate profile from memories"},
		{Name: "google", Description: "Re-authenticate Google (Gmail + Calendar)"},
		{Name: "metrics", Description: "View system metrics (e.g. /metrics slots 1h)"},
	}); err != nil {
		logger.Log.Warnf("Failed to set platform commands: %v", err)
	}

	// Send startup message to all allowed users.
	svc.Platform.Broadcast(context.Background(), "Sokratos is online.")

	eng := initEngine(cfg, svc, lb, registry)
	wired := wireEngine(cfg, svc, lb, eng, registry, tb)

	runStartupTasks()

	// Start the engine after all fields, callbacks, and startup DB state are
	// fully wired. Any goroutine started earlier (SubagentClient workers, etc.)
	// does not access eng fields, so this is the correct synchronization point.
	go eng.Run()

	confirmGate := make(map[string]bool, len(cfg.ConfirmTools))
	for _, t := range cfg.ConfirmTools {
		confirmGate[t] = true
	}
	confirmExec := confirmToolExec(registry, svc.Platform, confirmGate, cfg.ConfirmationTimeout)

	// Wire the engine with the confirmation-gated executor too, so heartbeat-triggered
	// tools (e.g. send_email from a routine) also require user approval.
	eng.ToolExec = confirmExec

	// If no router was created (3-model mode or missing deps), provide a
	// passthrough that always uses the primary orchestrator.
	router := wired.router
	if router == nil {
		router = engine.NewPassthroughRouter(lb.Client, cfg.LLMModel)
	}

	mc := messageContext{
		cfg:            cfg,
		svc:            svc,
		eng:            eng,
		lb:             lb,
		registry:       registry,
		triageCfg: tb.EmailTriageCfg,
		confirmExec:    confirmExec,
		skillMtimes:    wired.skillMtimes,
		skillDeps:      wired.skillDeps,
		rebuildGrammar: wired.rebuildGrammar,
		router:         router,
		delegateConfig: tb.DelegateConfig,
		platform:       svc.Platform,
	}

	for msg := range svc.Platform.Messages() {
		if len(cfg.AllowedIDs) > 0 {
			senderID, _ := strconv.ParseInt(msg.SenderID, 10, 64)
			if _, ok := cfg.AllowedIDs[senderID]; !ok {
				logger.Log.Warnf("Rejected message from %s: %q", msg.SenderTag, msg.Text)
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

		if msg.PhotoData != nil {
			dataURI := fmt.Sprintf("data:%s;base64,%s", msg.PhotoMIME, base64.StdEncoding.EncodeToString(msg.PhotoData))

			// Generate a text caption via the VL subagent so image context
			// survives text-only pipelines (triage, memory, archival).
			caption := msg.Text
			if caption == "" {
				caption = "What's in this image?"
			}
			if svc.Subagent != nil {
				capCtx, capCancel := context.WithTimeout(context.Background(), timeouts.SubagentCall)
				imgCaption, capErr := svc.Subagent.CaptionImage(capCtx, "Describe the image in one or two sentences. Be specific about key subjects, text, and context.", dataURI, 256)
				capCancel()
				if capErr != nil {
					logger.Log.Debugf("[photo] caption skipped: %v", capErr)
				} else if imgCaption = strings.TrimSpace(imgCaption); imgCaption != "" {
					caption = fmt.Sprintf("[Image: %s]\n%s", imgCaption, caption)
					logger.Log.Infof("[photo] captioned: %s", imgCaption)
				}
			}

			msgText = caption
			logger.Log.Infof("[%s] [photo] %s", msg.SenderTag, caption)

			userPrompt = caption + "\n\n[Current Agent State]\n" + stateCtx
			visionParts = []llm.ContentPart{
				{Type: "text", Text: userPrompt},
				{Type: "image_url", ImageURL: &llm.ImageURL{URL: dataURI}},
			}
		} else if msg.Text != "" {
			msgText = msg.Text
			logger.Log.Infof("[%s] %s", msg.SenderTag, msgText)

			userPrompt = msgText + "\n\n[Current Agent State]\n" + stateCtx
		} else {
			continue
		}

		// Direct-dispatch slash commands bypass the orchestrator entirely.
		trimmed := strings.TrimSpace(msgText)
		switch {
		case trimmed == "/reload":
			svc.Platform.Send(context.Background(), msg.ChannelID, handleReload(mc), msg.ID)
		case trimmed == "/bootstrap":
			svc.Platform.Send(context.Background(), msg.ChannelID, handleBootstrap(mc), msg.ID)
		case trimmed == "/google":
			svc.Platform.Send(context.Background(), msg.ChannelID, handleGoogle(mc), msg.ID)
		case trimmed == "/metrics" || strings.HasPrefix(trimmed, "/metrics "):
			args := strings.TrimPrefix(trimmed, "/metrics")
			svc.Platform.Send(context.Background(), msg.ChannelID, handleMetrics(mc, strings.TrimSpace(args)), msg.ID)
		default:
			processMessage(mc, msg, msgText, userPrompt, visionParts)
		}
	}
}
