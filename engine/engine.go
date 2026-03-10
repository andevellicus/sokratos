package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/metrics"
	"sokratos/timeouts"
	"sokratos/timefmt"
)

// GatekeeperClient is the interface for fast gatekeeper calls. Satisfied by
// *clients.SubagentClient — defined as an interface here to avoid a circular
// import between engine and clients.
type GatekeeperClient interface {
	CompleteWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error)
	TryCompleteWithGrammar(ctx context.Context, systemPrompt, userContent, grammar string, maxTokens int) (string, error)
}

// WorkMonitor tracks running work items (routines, background plans, scheduled
// tasks) and can kill hung work that exceeds its timeout. Satisfied by
// *tools.WorkTracker — defined as an interface to avoid circular imports.
type WorkMonitor interface {
	TrackStart(workType, directive string, timeout time.Duration) int64
	SetCancel(id int64, cancel context.CancelFunc)
	TrackEnd(id int64, status, errMsg string)
	KillHungWork() int
}

// LLMConfig groups model and orchestrator-related fields.
type LLMConfig struct {
	Client           *llm.Client
	Model            string
	ToolAgent        *llm.ToolAgentConfig // when set, enables the supervisor pattern
	MaxToolResultLen int                  // max chars per tool result (0 = default 2000)
	MaxWebSources    int                  // replaces %MAX_WEB_SOURCES% in system prompt (0 = default 2)
}

// CognitiveConfig groups event-driven cognitive processing data fields.
// LLM-dependent operations live on the CognitiveServices interface.
type CognitiveConfig struct {
	BufferThreshold           int           // min unreflected memories to trigger cognitive processing (default 20)
	LullDuration              time.Duration // min user idle time before cognitive processing (default 20min)
	Ceiling                   time.Duration // max time between cognitive runs (default 4h)
	ReflectionMemoryThreshold int           // run reflection after this many new memories (default 50, 0 = disabled)
	ReflectionPrompt          string        // system prompt for reflection synthesis
}

// MemoryFuncs groups the memory-related function dependencies.
type MemoryFuncs struct {
	DTCQueueFn  memory.WorkQueueFunc       // DTC work queue — preferred for distillation (less hallucination)
	SubagentFn  memory.SubagentFunc        // for conversation archive distillation (nil = skip distillation)
	GrammarFn   memory.GrammarSubagentFunc // for grammar-constrained quality scoring (nil = skip enrichment)
	BgGrammarFn memory.GrammarSubagentFunc // non-blocking, for contradiction checks + entity extraction
	QueueFn     memory.WorkQueueFunc       // background work queue for distillation/enrichment (nil = direct call)
}

// TTLConfig groups the TTL fields for periodic table pruning (0 = disabled).
type TTLConfig struct {
	MemoryStalenessDays    int
	WorkItemsTTLDays       int
	ProcessedEmailsTTLDays int
	ProcessedEventsTTLDays int
	FailedOpsTTLDays       int
	SkillKVTTLDays         int
	ShellHistoryTTLDays    int
	MetricsTTLDays         int
}

// CuriositySignal is an event-driven trigger for proactive research.
type CuriositySignal struct {
	Source      string // "conversation", "reflection", "objective"
	Query       string // information gap or research question
	Priority    int    // 3=low, 5=normal, 7=high
	ObjectiveID int64  // non-zero if tied to an objective
}

// Engine holds all dependencies for the heartbeat loop.
type Engine struct {
	LLM                LLMConfig
	Cognitive          CognitiveConfig
	ToolExec           func(context.Context, json.RawMessage) (string, error)
	Mu                 *sync.Mutex
	Interval           time.Duration
	RoutineInterval    time.Duration // polling interval for the routine scheduler (default 30s)
	SM                 *StateManager
	DB                 *pgxpool.Pool // nil when running without database
	EmbedEndpoint      string        // empty when embeddings unavailable
	EmbedModel         string        // model name for embedding endpoint
	MaxMessages        int           // context window cap for slide (default 40 via MAX_MESSAGES)
	PersonalityContent string        // personality traits markdown for system prompt injection
	ProfileContent     string        // identity profile JSON for system prompt injection
	TemporalContent    string        // temporal context XML for system prompt injection

	MaintenanceInterval time.Duration // interval between maintenance runs (decay, pruning); 0 = 30m default
	TTL                 TTLConfig     // periodic table pruning thresholds
	Memory              MemoryFuncs   // memory-related function dependencies

	Notifier       Notifier           // sends proactive messages to the user (nil = silent)
	InterruptChan  chan struct{}       // signals the task scheduler to recalculate
	Gatekeeper     GatekeeperClient   // fast gatekeeper for heartbeat Phase 2 (nil = use orchestrator)
	WorkMonitor    WorkMonitor        // tracks running work items; nil = no tracking
	RoutineTimeout time.Duration      // max duration for a single routine execution (default 5m)
	Router         SlotRouter         // routes orchestrator calls to Brain or subagent fallback; nil = use LLM.Client
	Reloader       HotReloader        // hot-reload skills + routines from disk (nil = skip)
	ReflectionSink ReflectionSink     // inject reflection insights into conversation context (nil = skip)
	CogServices    CognitiveServices  // LLM-dependent cognitive operations (nil = disabled)
	Metrics        *metrics.Collector // observability metrics (nil = disabled)
	OnFirstTick       func()                    // deferred startup work (e.g. consolidation) — runs after first heartbeat, nil = skip
	CuriositySignals  chan CuriositySignal      // buffered channel for event-driven curiosity (nil = disabled)

	// Configurable cooldowns (zero = use defaults).
	ObjectivePursuitCooldown   time.Duration
	ObjectiveInferenceCooldown time.Duration
	CuriosityCooldown          time.Duration

	// Internal timers (not configured externally).
	lastCognitiveRun           time.Time
	lastMaintenanceRun         time.Time
	lastCuriosityRun           time.Time
	lastObjectiveInferenceRun  time.Time
	lastObjectivePursuitRun    time.Time
	consolidateNudge   atomic.Bool // set by triage when a high-salience memory is saved; cleared after consolidation
	lastHeartbeatHash  [32]byte    // SHA-256 of last proactive heartbeat reply (dedup guard)
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

// RecordBackgroundCompletion records a background task completion as a recent
// action, safe for concurrent use from background goroutines.
func (e *Engine) RecordBackgroundCompletion(typ, summary string) {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	e.recordAction(typ, summary)
}

// FormatRecentActionsXML returns recent system actions (routines, heartbeats)
// as an XML block for injection into the interactive prompt. Actions older than
// maxAge are excluded. Returns empty string if nothing recent.
func (e *Engine) FormatRecentActionsXML(maxAge time.Duration) string {
	e.Mu.Lock()
	actions := make([]actionRecord, len(e.recentActions))
	copy(actions, e.recentActions)
	e.Mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	var sb strings.Builder
	for _, a := range actions {
		if a.Time.Before(cutoff) {
			continue
		}
		fmt.Fprintf(&sb, "<action type=%q time=%q>%s</action>\n", a.Type, timefmt.FormatDateTime(a.Time), a.Summary)
	}
	if sb.Len() == 0 {
		return ""
	}
	return "<recent_system_actions>\n" + sb.String() + "</recent_system_actions>"
}

// withOrchestratorLock runs fn while holding the orchestrator mutex.
func (e *Engine) withOrchestratorLock(fn func()) {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	fn()
}

// runOrchestrator acquires a slot (Brain or fallback), wires the
// acquire/release/reacquire callbacks, and runs a QueryOrchestrator call
// under the orchestrator lock. This eliminates the repeated boilerplate
// across routines, heartbeat, and scheduler call sites.
// The configure callback lets callers customise opts (e.g. set History)
// before the LLM call fires. It runs inside the orchestrator lock.
func (e *Engine) runOrchestrator(ctx context.Context, preferBrain bool, prompt string,
	configure func(opts *llm.QueryOrchestratorOpts)) (string, []llm.Message, error) {

	choice := e.resolveOrchestrator(ctx, preferBrain)
	acquired := true
	defer func() {
		if acquired {
			choice.Release()
		}
	}()

	var reply string
	var msgs []llm.Message
	var err error
	e.withOrchestratorLock(func() {
		opts := e.baseOrchestratorOpts()
		opts.OnToolStart = func(_ string) { choice.Release(); acquired = false }
		opts.OnToolEnd = func(reCtx context.Context) error {
			if reErr := choice.Reacquire(reCtx); reErr != nil {
				return reErr
			}
			acquired = true
			return nil
		}
		if configure != nil {
			configure(opts)
		}
		reply, msgs, err = llm.QueryOrchestrator(
			ctx, choice.Client, choice.Model, prompt,
			e.ToolExec, DefaultTrimFn, opts,
		)
	})
	return reply, msgs, err
}

// baseOrchestratorOpts returns the common QueryOrchestratorOpts shared across
// all orchestrator call sites (heartbeat, routines, scheduled tasks).
func (e *Engine) baseOrchestratorOpts() *llm.QueryOrchestratorOpts {
	return &llm.QueryOrchestratorOpts{
		PersonalityContent: e.PersonalityContent,
		ProfileContent:     e.ProfileContent,
		TemporalContext:    e.TemporalContent,
		MaxToolResultLen:   e.LLM.MaxToolResultLen,
		MaxWebSources:      e.LLM.MaxWebSources,
		ToolAgent:          e.LLM.ToolAgent,
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
	if e.Notifier != nil {
		e.Notifier.Send(text)
	}
	logger.Log.Infof("heartbeat: %s delivered", logLabel)
	return true
}

// resolveOrchestrator returns the client/model to use for an orchestrator call.
// In two-model mode, routes to Brain or subagent based on slot availability.
// When preferBrain is true, the Brain is tried first (interactive messages).
// When Router is nil, always uses the primary orchestrator.
func (e *Engine) resolveOrchestrator(ctx context.Context, preferBrain bool) OrchestratorChoice {
	if e.Router != nil {
		return e.Router.AcquireOrFallback(ctx, preferBrain, PriorityBackground)
	}
	return OrchestratorChoice{
		Client:          e.LLM.Client,
		Model:           e.LLM.Model,
		Release:         func() {},
		ReleaseReserved: func() {},
		Reacquire:       func(context.Context) error { return nil },
	}
}

// NudgeConsolidate signals that a high-salience memory was saved and
// consolidation should run on the next heartbeat tick (regardless of buffer count).
func (e *Engine) NudgeConsolidate() {
	e.consolidateNudge.Store(true)
}

// archiveDeps returns the ArchiveDeps for context sliding/archival.
func (e *Engine) archiveDeps() ArchiveDeps {
	return ArchiveDeps{
		DB: e.DB, EmbedEndpoint: e.EmbedEndpoint, EmbedModel: e.EmbedModel,
		MemoryFuncs: e.Memory,
	}
}

// Run starts the engine's background loops. Three independent loops run
// concurrently: (1) the heartbeat loop for contextual reasoning, maintenance,
// and cognitive processing; (2) the routine scheduler for executing due
// routines; and (3) the task scheduler for PostgreSQL-backed scheduled tasks.
// All three serialize LLM access through Mu.
// Intended to be called as a goroutine.
func (e *Engine) Run() {
	// Load identity profile and personality traits from DB on startup.
	e.RefreshProfile()
	e.RefreshPersonality()

	// Start the DB-backed task scheduler.
	if e.DB != nil && e.InterruptChan != nil {
		go e.runTaskScheduler()
	}

	// Start the independent routine scheduler.
	if e.DB != nil {
		go e.runRoutineScheduler()
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
