package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/timeouts"
)

// GatekeeperClient is the interface for fast gatekeeper calls. Satisfied by
// *tools.SubagentClient — defined as an interface here to avoid a circular
// import between engine and tools.
type GatekeeperClient interface {
	CompleteWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error)
}

// LLMConfig groups model and orchestrator-related fields.
type LLMConfig struct {
	Client           *llm.Client
	Model            string
	Grammar          string               // GBNF grammar for tool-call constraint
	ToolAgent        *llm.ToolAgentConfig // when set, enables the supervisor pattern
	Fallbacks        llm.FallbackMap      // deterministic fallback chains for failed tools
	MaxToolResultLen int                  // max chars per tool result (0 = default 2000)
	MaxWebSources    int                  // replaces %MAX_WEB_SOURCES% in system prompt (0 = default 2)
}

// CognitiveConfig groups event-driven cognitive processing fields.
type CognitiveConfig struct {
	BufferThreshold           int                                                                     // min unreflected memories to trigger cognitive processing (default 20)
	LullDuration              time.Duration                                                           // min user idle time before cognitive processing (default 20min)
	Ceiling                   time.Duration                                                           // max time between cognitive runs (default 4h)
	ConsolidateFunc           func(ctx context.Context) (int, error)                                  // wraps tools.ConsolidateCore; nil = skip
	ReflectionMemoryThreshold int                                                                     // run reflection after this many new memories (default 50, 0 = disabled)
	ReflectionPrompt          string                                                                  // system prompt for reflection synthesis
	SynthesizeFunc            func(ctx context.Context, systemPrompt, content string) (string, error) // LLM call for synthesis
	CuriosityFunc             CuriosityFunc                                                           // launches background research tasks (nil = disabled)
}

// Engine holds all dependencies for the heartbeat loop.
type Engine struct {
	LLM                LLMConfig
	Cognitive          CognitiveConfig
	ToolExec           func(context.Context, json.RawMessage) (string, error)
	Mu                 *sync.Mutex
	Interval           time.Duration
	SM                 *StateManager
	DB                 *pgxpool.Pool // nil when running without database
	EmbedEndpoint      string        // empty when embeddings unavailable
	EmbedModel         string        // model name for embedding endpoint
	MaxMessages        int           // context window cap for slide (e.g. 20)
	PersonalityContent string        // personality traits markdown for system prompt injection
	ProfileContent     string        // identity profile JSON for system prompt injection
	TemporalContent    string        // temporal context XML for system prompt injection

	MaintenanceInterval time.Duration        // interval between maintenance runs (decay, pruning); 0 = 30m default
	MemoryStalenessDays int                  // prune stale memories older than this many days (0 = disabled)
	SendFunc            func(text string)    // sends a message to the user via Telegram
	InterruptChan       chan struct{}        // signals the task scheduler to recalculate
	Gatekeeper          GatekeeperClient     // fast gatekeeper for heartbeat Phase 2 (nil = use orchestrator)
	SubagentFunc        memory.SubagentFunc        // for conversation archive distillation (nil = skip distillation)
	GrammarFunc         memory.GrammarSubagentFunc // for grammar-constrained quality scoring (nil = skip enrichment)
	BgGrammarFunc       memory.GrammarSubagentFunc // non-blocking, for contradiction checks + entity extraction
	QueueFunc           memory.WorkQueueFunc       // background work queue for distillation/enrichment (nil = direct call)
	SyncFunc             func()               // hot-reload skills + routines from disk (called each heartbeat tick)
	ReflectionNotifyFunc func(summary string) // inject reflection insights into conversation context (nil = skip)
	OnFirstTick          func()               // deferred startup work (e.g. consolidation) — runs after first heartbeat, nil = skip

	// Internal timers (not configured externally).
	lastCognitiveRun   time.Time
	lastMaintenanceRun time.Time
	lastCuriosityRun   time.Time
	lastHeartbeatHash  [32]byte // SHA-256 of last proactive heartbeat reply (dedup guard)
	recentActions      []actionRecord // last ≤5 actions taken (routines + heartbeat); no mutex — sequential callers only
}

// actionRecord captures a single action taken during a heartbeat tick.
type actionRecord struct {
	Time    time.Time
	Type    string // "routine" or "heartbeat"
	Summary string
}

// recordAction appends an action to the recent history, capping at 5 entries.
func (e *Engine) recordAction(typ, summary string) {
	e.recentActions = append(e.recentActions, actionRecord{Time: time.Now(), Type: typ, Summary: summary})
	if len(e.recentActions) > 5 {
		e.recentActions = e.recentActions[len(e.recentActions)-5:]
	}
}

// withOrchestratorLock runs fn while holding the orchestrator mutex.
func (e *Engine) withOrchestratorLock(fn func()) {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	fn()
}

// baseOrchestratorOpts returns the common QueryOrchestratorOpts shared across
// all orchestrator call sites (heartbeat, routines, scheduled tasks).
func (e *Engine) baseOrchestratorOpts() *llm.QueryOrchestratorOpts {
	return &llm.QueryOrchestratorOpts{
		Grammar:            e.LLM.Grammar,
		PersonalityContent: e.PersonalityContent,
		ProfileContent:     e.ProfileContent,
		TemporalContext:    e.TemporalContent,
		MaxToolResultLen:   e.LLM.MaxToolResultLen,
		MaxWebSources:      e.LLM.MaxWebSources,
		ToolAgent:          e.LLM.ToolAgent,
		Fallbacks:          e.LLM.Fallbacks,
	}
}

// sendDeduped sends text via SendFunc, suppressing consecutive identical messages.
// Returns true if the message was delivered, false if suppressed as a duplicate.
func (e *Engine) sendDeduped(text, logLabel string) bool {
	h := sha256.Sum256([]byte(text))
	if h == e.lastHeartbeatHash {
		logger.Log.Infof("heartbeat: suppressed duplicate %s", logLabel)
		return false
	}
	e.lastHeartbeatHash = h
	if e.SendFunc != nil {
		e.SendFunc(text)
	}
	logger.Log.Infof("heartbeat: %s delivered", logLabel)
	return true
}

// archiveDeps returns the ArchiveDeps for context sliding/archival.
func (e *Engine) archiveDeps() ArchiveDeps {
	return ArchiveDeps{DB: e.DB, EmbedEndpoint: e.EmbedEndpoint, EmbedModel: e.EmbedModel, SubagentFn: e.SubagentFunc, GrammarFn: e.GrammarFunc, BgGrammarFn: e.BgGrammarFunc, QueueFn: e.QueueFunc}
}

// Run starts a blocking loop that fires at the given interval. Each tick, it
// reads the current agent state, builds a heartbeat prompt with the state
// in Markdown, and sends it to the LLM orchestrator. Maintenance (decay,
// pruning) and cognitive processing (reflection, episode synthesis, profile
// consolidation) are evaluated within each heartbeat tick using volume + lull
// triggers. It serializes LLM access through Mu.
// If a database is available, it starts a PostgreSQL-backed task scheduler
// goroutine alongside the heartbeat loop.
// Intended to be called as a goroutine.
func (e *Engine) Run() {
	// Load identity profile and personality traits from DB on startup.
	e.RefreshProfile()
	e.RefreshPersonality()

	// Start the DB-backed task scheduler.
	if e.DB != nil && e.InterruptChan != nil {
		go e.runTaskScheduler()
	}

	e.lastCognitiveRun = time.Now()
	e.lastMaintenanceRun = time.Now()

	heartbeat := time.NewTicker(e.Interval)
	defer heartbeat.Stop()

	logger.Log.Infof("[engine] heartbeat started (interval: %s, buffer: %d, lull: %s, ceiling: %s)",
		e.Interval, e.Cognitive.BufferThreshold, e.Cognitive.LullDuration, e.Cognitive.Ceiling)

	firstTick := true
	for {
		<-heartbeat.C
		e.heartbeatTick()

		// Run deferred startup work (e.g. consolidation) after the first
		// heartbeat completes, so DTC is available for interactive requests
		// during startup instead of being blocked by consolidation.
		if firstTick && e.OnFirstTick != nil {
			go e.OnFirstTick()
			firstTick = false
		}
	}
}

// RefreshProfile loads the identity profile from the database into the engine's
// ProfileContent field. Called on startup and after consolidation runs.
func (e *Engine) RefreshProfile() {
	if e.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()
	content, err := memory.GetIdentityProfile(ctx, e.DB)
	if err != nil {
		logger.Log.Warnf("[engine] failed to refresh profile: %v", err)
		return
	}
	e.ProfileContent = content
}

// RefreshPersonality loads personality traits from the database into the engine's
// PersonalityContent field. Called on startup and after personality mutations.
func (e *Engine) RefreshPersonality() {
	if e.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()
	content, err := memory.FormatPersonalityForPrompt(ctx, e.DB)
	if err != nil {
		logger.Log.Warnf("[engine] failed to refresh personality: %v", err)
		return
	}
	e.PersonalityContent = content
}
