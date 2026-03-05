package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sokratos/logger"
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

// CuriosityFunc is called by the curiosity engine with a research directive.
// It should decompose and launch a background task. Returns the task ID.
// objectiveID is non-zero when launching for a specific objective (0 = curiosity/no objective).
type CuriosityFunc func(directive string, priority int, objectiveID int64) (int64, error)

// runCuriosityIfReady checks conditions and optionally fires a proactive
// research task. Inserted as a phase in cognitive processing.
func (e *Engine) runCuriosityIfReady() {
	if e.Cognitive.CuriosityFunc == nil || e.DB == nil || e.Gatekeeper == nil {
		return
	}

	// Cooldown check.
	cooldown := e.CuriosityCooldown
	if cooldown == 0 {
		cooldown = defaultCuriosityCooldown
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
	prompt := `You are generating a proactive research question based on recent conversation patterns.
Given recent memory summaries, identify ONE knowledge gap or interesting tangent worth exploring.
Output JSON: {"directive": "<specific research task>", "reasoning": "<why this is worth exploring>"}
Rules:
- The directive must be actionable via search_web + read_url tools.
- Focus on topics the user has shown interest in but where knowledge is incomplete.
- Do NOT repeat research that has already been done (check the summaries).
- If active objectives exist, STRONGLY prefer research that advances one of them. Reference which objective the research serves in the directive.
- Only explore random tangents if no active objectives exist or none have actionable research angles.
- If nothing is worth researching, output: {"directive": "", "reasoning": "no gaps found"}`

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

	raw, err := e.Gatekeeper.CompleteWithGrammar(ctx, prompt, userContent, grammarStr, tokens.GatekeeperDecision)
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
	taskID, err := e.Cognitive.CuriosityFunc(directive, 3, 0) // low priority, no objective
	if err != nil {
		logger.Log.Warnf("[curiosity] failed to launch: %v", err)
		return
	}

	e.lastCuriosityRun = time.Now()
	logger.Log.Infof("[curiosity] launched task #%d: %s (reason: %s)", taskID, result.Directive, result.Reasoning)
}
