package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sokratos/adaptive"
	"sokratos/logger"
	"sokratos/prompts"
	"sokratos/memory"
	objpkg "sokratos/objectives"
	"sokratos/textutil"
	"sokratos/timeouts"
	"sokratos/tokens"
)

const (
	defaultCuriosityCooldown = 2 * time.Hour // min time between curiosity runs
	curiosityMaxInactive     = 6 * time.Hour // only trigger if user was active within this window
	curiosityMinMemories     = 15            // min recent memories to have enough signal
)

// runCuriosityIfReady checks conditions and optionally fires a proactive
// research task. Inserted as a phase in cognitive processing.
func (e *Engine) runCuriosityIfReady() {
	if e.CogServices == nil || e.DB == nil || e.Gatekeeper == nil {
		return
	}

	// Cooldown check — use adaptive parameter if DB is available.
	cooldown := e.CuriosityCooldown
	if cooldown == 0 {
		cooldown = defaultCuriosityCooldown
	}
	if e.DB != nil {
		hours := adaptive.Get(context.Background(), e.DB, "curiosity_cooldown_hours", cooldown.Hours())
		cooldown = time.Duration(hours * float64(time.Hour))
	}
	if time.Since(e.lastCuriosityRun) < cooldown {
		return
	}

	// User activity check — only be curious when the user was recently active.
	lastActivity := e.SM.LastUserActivity()
	if lastActivity.IsZero() || time.Since(lastActivity) > curiosityMaxInactive {
		return
	}

	// Check for already-running curiosity tasks.
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.DBQuery)
	defer cancel()
	var running int
	if err := e.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_items
		 WHERE status = 'running' AND directive LIKE '[curiosity]%'`).Scan(&running); err == nil && running > 0 {
		logger.Log.Debug("[curiosity] task already running, skipping")
		return
	}

	// Query recent memories for signal.
	summaries, err := memory.QueryRecentSummaries(ctx, e.DB, 48, memory.SalienceLow, 20)
	if err != nil {
		return
	}

	if len(summaries) < curiosityMinMemories {
		return
	}

	// Query active objectives from the objectives table.
	activeObjectives, _ := objpkg.ListActive(ctx, e.DB)

	// Ask the gatekeeper (Flash) to generate a research question.
	prompt := prompts.CuriosityGatekeeper

	// Build user content with objectives prepended.
	var uc strings.Builder
	if len(activeObjectives) > 0 {
		uc.WriteString("Active objectives:\n")
		for i, g := range activeObjectives {
			fmt.Fprintf(&uc, "%d. %s\n", i+1, g.Summary)
		}
		uc.WriteString("\n")
	}
	uc.WriteString("Recent memories:\n")
	uc.WriteString(strings.Join(summaries, "\n"))
	userContent := uc.String()
	grammarStr := `root ::= "{" ws "\"directive\":" ws string "," ws "\"reasoning\":" ws string ws "}"
string ::= "\"" chars "\""
chars ::= char*
char ::= [^"\\] | "\\" escape
escape ::= ["\\nrt/]
ws ::= [ \t\n]*`

	raw, err := e.Gatekeeper.TryCompleteWithGrammar(ctx, prompt, userContent, grammarStr, tokens.GatekeeperDecision)
	if err != nil {
		logger.Log.Debugf("[curiosity] gatekeeper skipped (slot busy or error): %v", err)
		return
	}

	var result struct {
		Directive string `json:"directive"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(textutil.CleanLLMJSON(raw)), &result); err != nil || result.Directive == "" {
		logger.Log.Debug("[curiosity] no research question generated")
		return
	}

	// Launch background research task.
	directive := fmt.Sprintf("[curiosity] %s", result.Directive)
	taskID, err := e.CogServices.LaunchCuriosity(directive, 3, 0) // low priority, no objective
	if err != nil {
		logger.Log.Warnf("[curiosity] failed to launch: %v", err)
		return
	}

	e.lastCuriosityRun = time.Now()
	e.Metrics.Emit("curiosity.signal", 1, map[string]string{"source": "cognitive", "result": "emitted"})
	logger.Log.Infof("[curiosity] launched task #%d: %s (reason: %s)", taskID, result.Directive, result.Reasoning)
}

// drainCuriositySignals non-blocking drains all pending signals from the
// CuriositySignals channel, keeps the highest priority one, and launches it.
// Respects the same cooldown as timer-based curiosity.
func (e *Engine) drainCuriositySignals() {
	if e.CuriositySignals == nil || e.CogServices == nil {
		return
	}

	// Non-blocking drain: collect all pending signals.
	var best *CuriositySignal
	for {
		select {
		case sig := <-e.CuriositySignals:
			if best == nil || sig.Priority > best.Priority {
				best = &sig
			}
		default:
			goto done
		}
	}
done:
	if best == nil {
		return
	}

	// Respect cooldown (same as timer-based curiosity).
	cooldown := e.CuriosityCooldown
	if cooldown == 0 {
		cooldown = defaultCuriosityCooldown
	}
	if e.DB != nil {
		hours := adaptive.Get(context.Background(), e.DB, "curiosity_cooldown_hours", cooldown.Hours())
		cooldown = time.Duration(hours * float64(time.Hour))
	}
	// Signals use a shorter minimum gap (30 min) instead of the full cooldown.
	minGap := 30 * time.Minute
	if cooldown < minGap {
		minGap = cooldown
	}
	if time.Since(e.lastCuriosityRun) < minGap {
		return
	}

	directive := fmt.Sprintf("[curiosity:%s] %s", best.Source, best.Query)
	taskID, err := e.CogServices.LaunchCuriosity(directive, best.Priority, best.ObjectiveID)
	if err != nil {
		logger.Log.Warnf("[curiosity-signal] failed to launch: %v", err)
		e.Metrics.Emit("curiosity.signal", 1, map[string]string{"source": best.Source, "result": "dropped"})
		return
	}

	e.lastCuriosityRun = time.Now()
	e.Metrics.Emit("curiosity.signal", 1, map[string]string{"source": best.Source, "result": "emitted"})
	logger.Log.Infof("[curiosity-signal] launched task #%d from %s: %s", taskID, best.Source, best.Query)
}
