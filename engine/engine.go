package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/timefmt"
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

	MaintenanceInterval time.Duration        // interval between maintenance runs (decay, pruning); 0 = 30m default
	MemoryStalenessDays int                  // prune stale memories older than this many days (0 = disabled)
	SendFunc            func(text string)    // sends a message to the user via Telegram
	InterruptChan       chan struct{}        // signals the task scheduler to recalculate
	Gatekeeper          GatekeeperClient     // fast gatekeeper for heartbeat Phase 2 (nil = use orchestrator)
	SubagentFunc        memory.SubagentFunc        // for conversation archive distillation (nil = skip distillation)
	GrammarFunc         memory.GrammarSubagentFunc // for grammar-constrained quality scoring (nil = skip enrichment)
	QueueFunc           memory.WorkQueueFunc       // background work queue for distillation/enrichment (nil = direct call)
	OnFirstTick         func()               // deferred startup work (e.g. consolidation) — runs after first heartbeat, nil = skip

	// Internal timers (not configured externally).
	lastCognitiveRun   time.Time
	lastMaintenanceRun time.Time
	lastHeartbeatHash  [32]byte // SHA-256 of last proactive heartbeat reply (dedup guard)
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
	if e.SendFunc != nil {
		e.SendFunc(text)
	}
	logger.Log.Infof("heartbeat: %s delivered", logLabel)
	return true
}

// archiveDeps returns the ArchiveDeps for context sliding/archival.
func (e *Engine) archiveDeps() ArchiveDeps {
	return ArchiveDeps{DB: e.DB, EmbedEndpoint: e.EmbedEndpoint, EmbedModel: e.EmbedModel, SubagentFn: e.SubagentFunc, GrammarFn: e.GrammarFunc, QueueFn: e.QueueFunc}
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
		// heartbeat completes, so Z1 is available for interactive requests
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

// heartbeatPrefetch embeds the current task and retrieves semantically similar
// memories as background context for the heartbeat. Returns nil if the current
// task is empty, embedding fails, or no memories match.
func (e *Engine) heartbeatPrefetch(ctx context.Context) *llm.Message {
	task := e.SM.GetState().CurrentTask
	if strings.TrimSpace(task) == "" {
		return nil
	}

	embedCtx, cancel := context.WithTimeout(ctx, timeouts.Embedding)
	defer cancel()

	pf := memory.Prefetch(embedCtx, e.DB, e.EmbedEndpoint, e.EmbedModel, task, task, 3)
	if pf == nil {
		return nil
	}

	// Bump retrieval stats in background.
	go func() {
		bCtx, bCancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
		defer bCancel()
		memory.TrackRetrieval(bCtx, e.DB, pf.IDs)
	}()

	logger.Log.Infof("[engine] heartbeat prefetch injected %d memories", len(pf.IDs))
	return &llm.Message{Role: "user", Content: pf.Content}
}

// dueRoutine represents a single routine row that's due for execution.
type dueRoutine struct {
	ID          int
	Name        string
	Instruction string
}

// heartbeatTask represents a pending task for heartbeat context assembly.
type heartbeatTask struct {
	ID          int64
	Description string
	DueAt       *time.Time
}

// heartbeatMemory represents a recent salient memory for heartbeat context.
type heartbeatMemory struct {
	Summary   string
	CreatedAt time.Time
}

// backgroundTask represents a background plan_and_execute task for heartbeat context.
type backgroundTask struct {
	ID        int64
	Directive string
	Status    string
	Priority  int
	Progress  string // "2/5"
	ErrMsg    *string
}

// heartbeatContext holds working memory gathered from Postgres for Phase 2.
type heartbeatContext struct {
	currentTime      string
	currentObjective string // from e.SM.GetState().CurrentTask
	userLastActive   string // RFC3339 timestamp of last user message
	tasks            []heartbeatTask
	memories         []heartbeatMemory
	backgroundTasks  []backgroundTask
}

// executivePromptBase is the template for the Phase 2 system message.
// It contains a %s placeholder for staleness context (populated per-tick).
const executivePromptBase = `You are running your autonomous background loop. Routines have already been executed for this tick. Review the <heartbeat_context> and determine if any proactive action is required.

%s

Priority order:
1. Pending tasks that are overdue or due within 24 hours — take action if you can make progress without user input.
2. If recent memories suggest something time-sensitive needs attention (upcoming travel, an open promise to follow up, an approaching deadline), address it.
3. Only message the user if there is something they need to know or decide RIGHT NOW. Do not message the user to report that background tasks completed silently.

ABSOLUTE RULES:
- Do NOT continue, revisit, or follow up on previous conversations. The conversation is NOT your concern here.
- Do NOT repeat information you have already told the user.
- Do NOT summarize what happened in previous conversations.
- Do NOT offer unsolicited help ("Would you like me to...", "I can also...").

CRITICAL: You MUST respond with exactly ONE of these two formats — anything else will be sent directly to the user as a Telegram message:
- If no action is needed: <NO_ACTION_REQUIRED>
- If action is needed: <TOOL_INTENT>describe the exact action to take</TOOL_INTENT>

Do NOT output status words like "idle", acknowledgements, or commentary. Your entire response must be one of the two tags above.`

// gatekeeperGrammar is a GBNF grammar constraining gatekeeper output to one of
// three JSON decision shapes: no action, tool intent, or direct message.
const gatekeeperGrammar = `root ::= none | tool | message
none ::= "{" ws "\"action\":" ws "\"none\"" ws "}"
tool ::= "{" ws "\"action\":" ws "\"tool\"" ws "," ws "\"intent\":" ws string ws "}"
message ::= "{" ws "\"action\":" ws "\"message\"" ws "," ws "\"text\":" ws string ws "}"
string ::= "\"" chars "\""
chars ::= char*
char ::= [^"\\] | "\\" escape
escape ::= ["\\nrt/]
ws ::= [ \t\n]*`

// gatekeeperPromptBase is the system prompt for the fast gatekeeper.
// It contains a %s placeholder for staleness context.
const gatekeeperPromptBase = `You are a background heartbeat gatekeeper. Your job is to evaluate the provided context and decide whether proactive action is needed.

%s

Respond with exactly ONE JSON object:
- {"action": "none"} — no action needed (the default; use this ~90%% of the time).
- {"action": "tool", "intent": "..."} — a tool-based action is needed. Describe the intent concisely.
- {"action": "message", "text": "..."} — send a short message directly to the user (only for truly urgent, time-sensitive items).

Rules:
- Do NOT continue, revisit, or follow up on previous conversations.
- Do NOT repeat information the user already knows.
- Do NOT offer unsolicited help.
- Prefer "none" unless there is a clear, actionable, time-sensitive reason to act.
- A pending task is only actionable if it is overdue or due within the next hour AND you can make progress without user input.`

// gatekeeperDecision represents the parsed gatekeeper JSON output.
type gatekeeperDecision struct {
	Action string `json:"action"`
	Intent string `json:"intent,omitempty"`
	Text   string `json:"text,omitempty"`
}

// heartbeatTick handles a single heartbeat using a two-phase approach:
// Phase 1 executes due routines deterministically, then Phase 2 runs
// contextual orchestrator reasoning over working memory.
func (e *Engine) heartbeatTick() {
	defer func() {
		if r := recover(); r != nil {
			logger.Log.Errorf("heartbeat: panic recovered: %v", r)
		}
	}()

	if e.LLM.Client == nil {
		logger.Log.Warn("heartbeat: llm.Client is nil, skipping tick")
		return
	}

	// Refresh user preferences from DB (picks up externally added prefs).
	e.SM.RefreshPrefs()

	// === PHASE 1: Deterministic Routine Execution ===
	routinesFired := 0
	if e.DB != nil {
		routinesFired = e.executeDueRoutines()
	}

	// === PHASE 2: Contextual Reasoning ===
	hbCtx := e.gatherHeartbeatContext()
	contextXML := hbCtx.toXML()

	// Staleness detection: if the user hasn't sent a message recently,
	// exclude conversation history to prevent the model from trying to
	// continue or rehash stale conversations.
	lastActivity := e.SM.LastUserActivity()
	staleThreshold := 2 * e.Interval
	if staleThreshold < 10*time.Minute {
		staleThreshold = 10 * time.Minute
	}
	conversationStale := !lastActivity.IsZero() && time.Since(lastActivity) > staleThreshold

	// Build staleness context.
	var stalenessNote string
	if lastActivity.IsZero() {
		stalenessNote = "The user has not sent any messages this session. Do NOT initiate conversation."
	} else if conversationStale {
		stalenessNote = fmt.Sprintf("The user has been inactive for %s. The conversation is STALE — do NOT continue, revisit, or follow up on it.", time.Since(lastActivity).Truncate(time.Minute))
	} else {
		stalenessNote = "The user is actively chatting. Only take proactive action if truly urgent."
	}

	if e.Gatekeeper != nil {
		e.heartbeatPhase2Gatekeeper(contextXML, stalenessNote, conversationStale)
	} else {
		e.heartbeatPhase2Orchestrator(contextXML, stalenessNote, conversationStale)
	}

	if e.DB != nil && e.EmbedEndpoint != "" {
		SlideAndArchiveContext(context.Background(), e.SM, e.MaxMessages, e.archiveDeps())
	}

	// Phase 3: Periodic maintenance (decay + pruning).
	e.runMaintenanceIfDue()

	// Phase 4: Event-driven cognitive processing.
	e.runCognitiveIfTriggered()

	logger.Log.Infof("heartbeat: tick complete, routines_fired=%d", routinesFired)
}

// heartbeatPhase2Gatekeeper runs Phase 2 via the fast gatekeeper (subagent/Flash).
// Only escalates to the orchestrator when the gatekeeper decides action is needed.
// Falls back to the orchestrator path on any gatekeeper error.
func (e *Engine) heartbeatPhase2Gatekeeper(contextXML, stalenessNote string, conversationStale bool) {
	prompt := fmt.Sprintf(gatekeeperPromptBase, stalenessNote)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := e.Gatekeeper.CompleteWithGrammar(ctx, prompt, contextXML, gatekeeperGrammar, 256)
	if err != nil {
		logger.Log.Warnf("heartbeat: gatekeeper error, falling back to orchestrator: %v", err)
		e.heartbeatPhase2Orchestrator(contextXML, stalenessNote, conversationStale)
		return
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		logger.Log.Info("heartbeat: gatekeeper returned empty response, treating as no action")
		return
	}

	var decision gatekeeperDecision
	if err := json.Unmarshal([]byte(raw), &decision); err != nil {
		logger.Log.Warnf("heartbeat: gatekeeper parse error, falling back to orchestrator: %v (raw: %s)", err, raw)
		e.heartbeatPhase2Orchestrator(contextXML, stalenessNote, conversationStale)
		return
	}

	switch decision.Action {
	case "none":
		logger.Log.Info("heartbeat: gatekeeper decided no action required")

	case "tool":
		logger.Log.Infof("heartbeat: gatekeeper requested tool action, routing to orchestrator: %s", decision.Intent)
		toolPrompt := fmt.Sprintf(
			"%s\n\nROUTINE: %s\nExecute this action now. Use your tools to complete it.\n"+
				"Do not message the user unless the action explicitly requires it.",
			contextXML, decision.Intent,
		)

		var reply string
		var msgs []llm.Message
		var orchestratorErr error
		e.withOrchestratorLock(func() {
			opts := e.baseOrchestratorOpts()
			if !conversationStale {
				opts.History = e.SM.ReadMessages()
			}
			reply, msgs, orchestratorErr = llm.QueryOrchestrator(
				context.Background(), e.LLM.Client, e.LLM.Model, toolPrompt,
				e.ToolExec, DefaultTrimFn, opts,
			)
		})

		if !conversationStale {
			for _, m := range msgs {
				e.SM.AppendMessage(m)
			}
		}

		if orchestratorErr != nil {
			logger.Log.Errorf("heartbeat: orchestrator error (gatekeeper-routed): %v", orchestratorErr)
		} else if reply = strings.TrimSpace(reply); reply != "" && !strings.Contains(reply, "<NO_ACTION_REQUIRED>") {
			e.sendDeduped(reply, "proactive response (gatekeeper-routed)")
		}

	case "message":
		text := strings.TrimSpace(decision.Text)
		if text == "" {
			logger.Log.Debug("heartbeat: gatekeeper returned empty message, ignoring")
			break
		}
		e.sendDeduped(text, "gatekeeper message")

	default:
		logger.Log.Warnf("heartbeat: gatekeeper returned unknown action %q, ignoring", decision.Action)
	}
}

// heartbeatPhase2Orchestrator runs Phase 2 via the full orchestrator (original path).
func (e *Engine) heartbeatPhase2Orchestrator(contextXML, stalenessNote string, conversationStale bool) {
	executivePrompt := fmt.Sprintf(executivePromptBase, stalenessNote)

	var reply string
	var msgs []llm.Message
	var err error
	e.withOrchestratorLock(func() {
		var history []llm.Message
		if !conversationStale {
			convHistory := e.SM.ReadMessages()
			history = make([]llm.Message, 0, len(convHistory)+1)
			history = append(history, convHistory...)
		} else {
			history = make([]llm.Message, 0, 1)
		}
		history = append(history, llm.Message{
			Role:    "user",
			Content: "[EXECUTIVE ROUTINE]\n" + executivePrompt,
		})

		opts := e.baseOrchestratorOpts()
		opts.History = history
		reply, msgs, err = llm.QueryOrchestrator(
			context.Background(), e.LLM.Client, e.LLM.Model, contextXML,
			e.ToolExec, DefaultTrimFn, opts,
		)
	})

	// Only persist Phase 2 messages when the conversation is active.
	if !conversationStale {
		for _, m := range msgs {
			e.SM.AppendMessage(m)
		}
	}

	switch {
	case err != nil:
		if strings.Contains(err.Error(), "too many tool call rounds") {
			logger.Log.Warn("heartbeat: max rounds reached")
			if e.SendFunc != nil {
				e.SendFunc("I started a background task but couldn't complete it. You may want to check in.")
			}
		} else {
			logger.Log.Errorf("heartbeat: orchestrator error: %v", err)
		}
	case strings.Contains(reply, "<NO_ACTION_REQUIRED>"):
		logger.Log.Info("heartbeat: no action required")
	case strings.TrimSpace(reply) != "":
		e.sendDeduped(strings.TrimSpace(reply), "proactive response")
	default:
		logger.Log.Debug("heartbeat: orchestrator produced unexpected output")
	}
}

// gatherHeartbeatContext queries Postgres for pending tasks and recent salient
// memories. Each query uses a 5-second timeout. On any query failure the
// affected section is left empty — the orchestrator still runs with partial context.
func (e *Engine) gatherHeartbeatContext() heartbeatContext {
	hc := heartbeatContext{
		currentTime:      timefmt.Now(),
		currentObjective: e.SM.GetState().CurrentTask,
	}
	if la := e.SM.LastUserActivity(); !la.IsZero() {
		hc.userLastActive = la.Format(time.RFC3339)
	}

	if e.DB == nil {
		return hc
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()

	// Query 1: Pending tasks.
	taskRows, err := e.DB.Query(queryCtx,
		`SELECT id, description, due_at
		 FROM tasks
		 WHERE status = 'pending'
		 ORDER BY due_at ASC NULLS LAST
		 LIMIT 5`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query pending tasks: %v", err)
	} else {
		for taskRows.Next() {
			var t heartbeatTask
			if err := taskRows.Scan(&t.ID, &t.Description, &t.DueAt); err != nil {
				logger.Log.Warnf("heartbeat: failed to scan task row: %v", err)
				continue
			}
			hc.tasks = append(hc.tasks, t)
		}
		taskRows.Close()
	}

	// Query 2: Recent salient memories (exclude backfill — those are historical
	// content ingested now, not things that actually happened recently).
	memRows, err := e.DB.Query(queryCtx,
		`SELECT summary, created_at
		 FROM memories
		 WHERE created_at >= NOW() - INTERVAL '48 hours'
		   AND salience >= 7
		   AND COALESCE(source, '') != 'backfill'
		 ORDER BY created_at DESC
		 LIMIT 3`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query recent memories: %v", err)
	} else {
		for memRows.Next() {
			var m heartbeatMemory
			if err := memRows.Scan(&m.Summary, &m.CreatedAt); err != nil {
				logger.Log.Warnf("heartbeat: failed to scan memory row: %v", err)
				continue
			}
			hc.memories = append(hc.memories, m)
		}
		memRows.Close()
	}

	// Query 3: Background tasks (running + recently completed within 1h).
	bgRows, err := e.DB.Query(queryCtx,
		`SELECT id, directive, status, COALESCE(priority, 5), steps_total, steps_completed, error_message
		 FROM background_tasks
		 WHERE status = 'running'
		    OR (status IN ('completed', 'failed') AND completed_at >= NOW() - INTERVAL '1 hour')
		 ORDER BY
		    CASE WHEN status = 'running' THEN 0 ELSE 1 END,
		    priority DESC,
		    created_at DESC
		 LIMIT 5`)
	if err != nil {
		logger.Log.Warnf("heartbeat: failed to query background tasks: %v", err)
	} else {
		for bgRows.Next() {
			var bt backgroundTask
			var stepsTotal, stepsCompleted int
			if err := bgRows.Scan(&bt.ID, &bt.Directive, &bt.Status, &bt.Priority, &stepsTotal, &stepsCompleted, &bt.ErrMsg); err != nil {
				logger.Log.Warnf("heartbeat: failed to scan background task row: %v", err)
				continue
			}
			bt.Progress = fmt.Sprintf("%d/%d", stepsCompleted, stepsTotal)
			hc.backgroundTasks = append(hc.backgroundTasks, bt)
		}
		bgRows.Close()
	}

	return hc
}

// toXML formats the heartbeat context as a dense XML block. Empty sections
// use "none" rather than being omitted so the orchestrator sees a consistent
// structure on every tick.
func (hc heartbeatContext) toXML() string {
	var b strings.Builder
	b.WriteString("<heartbeat_context>\n")
	fmt.Fprintf(&b, "  <current_time>%s</current_time>\n", hc.currentTime)

	objective := hc.currentObjective
	if objective == "" {
		objective = "none"
	}
	fmt.Fprintf(&b, "  <current_objective>%s</current_objective>\n", objective)

	lastActive := hc.userLastActive
	if lastActive == "" {
		lastActive = "never"
	}
	fmt.Fprintf(&b, "  <user_last_active>%s</user_last_active>\n", lastActive)

	// Recent salient memories.
	if len(hc.memories) == 0 {
		b.WriteString("  <recent_salient_memories>none</recent_salient_memories>\n")
	} else {
		b.WriteString("  <recent_salient_memories>\n")
		for _, m := range hc.memories {
			fmt.Fprintf(&b, "    <memory recorded=\"%s\">%s</memory>\n",
				m.CreatedAt.Format(time.RFC3339), m.Summary)
		}
		b.WriteString("  </recent_salient_memories>\n")
	}

	// Pending tasks.
	if len(hc.tasks) == 0 {
		b.WriteString("  <pending_tasks>none</pending_tasks>\n")
	} else {
		b.WriteString("  <pending_tasks>\n")
		for _, t := range hc.tasks {
			due := "none"
			if t.DueAt != nil {
				due = t.DueAt.Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "    <task id=\"%d\" due=\"%s\">%s</task>\n",
				t.ID, due, t.Description)
		}
		b.WriteString("  </pending_tasks>\n")
	}

	// Background tasks.
	if len(hc.backgroundTasks) == 0 {
		b.WriteString("  <background_tasks>none</background_tasks>\n")
	} else {
		b.WriteString("  <background_tasks>\n")
		for _, bt := range hc.backgroundTasks {
			errAttr := ""
			if bt.ErrMsg != nil && *bt.ErrMsg != "" {
				errAttr = fmt.Sprintf(" error=%q", *bt.ErrMsg)
			}
			dir := bt.Directive
			if len(dir) > 80 {
				dir = dir[:77] + "..."
			}
			fmt.Fprintf(&b, "    <bg_task id=\"%d\" status=\"%s\" priority=\"%d\" progress=\"%s\"%s>%s</bg_task>\n",
				bt.ID, bt.Status, bt.Priority, bt.Progress, errAttr, dir)
		}
		b.WriteString("  </background_tasks>\n")
	}

	b.WriteString("</heartbeat_context>")
	return b.String()
}

