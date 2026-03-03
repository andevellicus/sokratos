package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/textutil"
	"sokratos/timeouts"
)

const (
	curiosityCooldown    = 4 * time.Hour // min time between curiosity runs
	curiosityMaxInactive = 6 * time.Hour // only trigger if user was active within this window
	curiosityMinMemories = 15            // min recent memories to have enough signal
)

// CuriosityFunc is called by the curiosity engine with a research directive.
// It should decompose and launch a background task. Returns the task ID.
type CuriosityFunc func(directive string, priority int) (int64, error)

// runCuriosityIfReady checks conditions and optionally fires a proactive
// research task. Inserted as a phase in cognitive processing.
func (e *Engine) runCuriosityIfReady() {
	if e.Cognitive.CuriosityFunc == nil || e.DB == nil || e.Gatekeeper == nil {
		return
	}

	// Cooldown check.
	if time.Since(e.lastCuriosityRun) < curiosityCooldown {
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

	// Ask the gatekeeper (Flash) to generate a research question.
	prompt := `You are generating a proactive research question based on recent conversation patterns.
Given recent memory summaries, identify ONE knowledge gap or interesting tangent worth exploring.
Output JSON: {"directive": "<specific research task>", "reasoning": "<why this is worth exploring>"}
Rules:
- The directive must be actionable via search_web + read_url tools.
- Focus on topics the user has shown interest in but where knowledge is incomplete.
- Do NOT repeat research that has already been done (check the summaries).
- If nothing is worth researching, output: {"directive": "", "reasoning": "no gaps found"}`

	userContent := "Recent memories:\n" + strings.Join(summaries, "\n")
	grammarStr := `root ::= "{" ws "\"directive\":" ws string "," ws "\"reasoning\":" ws string ws "}"
string ::= "\"" chars "\""
chars ::= char*
char ::= [^"\\] | "\\" escape
escape ::= ["\\nrt/]
ws ::= [ \t\n]*`

	raw, err := e.Gatekeeper.CompleteWithGrammar(ctx, prompt, userContent, grammarStr, 256)
	if err != nil {
		logger.Log.Warnf("[curiosity] gatekeeper error: %v", err)
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
	taskID, err := e.Cognitive.CuriosityFunc(directive, 3) // low priority
	if err != nil {
		logger.Log.Warnf("[curiosity] failed to launch: %v", err)
		return
	}

	e.lastCuriosityRun = time.Now()
	logger.Log.Infof("[curiosity] launched task #%d: %s (reason: %s)", taskID, result.Directive, result.Reasoning)
}
