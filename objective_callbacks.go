package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/engine"
	"sokratos/logger"
	"sokratos/objectives"
	"sokratos/textutil"
	"sokratos/timeouts"
	"sokratos/tokens"
	"sokratos/tools"
)

// objectiveProgressEval is the grammar-constrained output from the subagent.
type objectiveProgressEval struct {
	Progress        string `json:"progress"`
	ObjectiveStatus string `json:"objective_status"` // continue, completed, stuck
	FollowUp        string `json:"follow_up"`
}

// buildObjectiveTaskCallback returns an OnObjectiveTaskComplete handler that evaluates
// objective progress via the subagent and updates the objectives table.
func buildObjectiveTaskCallback(db *pgxpool.Pool, sc *clients.SubagentClient, eng *engine.Engine) func(tools.ObjectiveTaskResult) {
	if db == nil {
		return nil
	}
	return func(r tools.ObjectiveTaskResult) {
		ctx, cancel := context.WithTimeout(context.Background(), timeouts.ObjectiveEval)
		defer cancel()

		// Append raw result as progress note regardless of evaluation.
		truncResult := textutil.Truncate(r.Result, 500)
		note := r.Status + ": " + truncResult
		objectives.AppendProgress(ctx, db, r.ObjectiveID, note)

		// Try to evaluate via subagent if available.
		if sc == nil {
			return
		}

		systemPrompt := `Evaluate the result of a background task that was pursuing an objective.
Output JSON: {"progress": "<what was accomplished>", "objective_status": "continue|completed|stuck", "follow_up": "<next step if continue, empty otherwise>"}
Rules:
- "completed" means the objective has been fully achieved.
- "continue" means progress was made but more work is needed.
- "stuck" means no meaningful progress was made or repeated failures.
- Keep progress summary under 100 words.`

		obj, err := objectives.Get(ctx, db, r.ObjectiveID)
		if err != nil {
			return
		}

		userContent := "Objective: " + obj.Summary + "\nTask: " + r.Directive + "\nStatus: " + r.Status + "\nResult:\n" + truncResult

		grammarStr := `root ::= "{" ws "\"progress\":" ws string "," ws "\"objective_status\":" ws status "," ws "\"follow_up\":" ws string ws "}"
status ::= "\"continue\"" | "\"completed\"" | "\"stuck\""
string ::= "\"" chars "\""
chars ::= char*
char ::= [^"\\] | "\\" escape
escape ::= ["\\nrt/]
ws ::= [ \t\n]*`

		raw, err := sc.TryCompleteWithGrammar(ctx, systemPrompt, userContent, grammarStr, tokens.ObjectiveEval)
		if err != nil {
			logger.Log.Debugf("[objective-callback] subagent unavailable for eval: %v", err)
			return
		}

		var eval objectiveProgressEval
		if err := json.Unmarshal([]byte(textutil.CleanLLMJSON(raw)), &eval); err != nil {
			logger.Log.Warnf("[objective-callback] failed to parse eval: %v", err)
			return
		}

		// Append evaluated progress note.
		if eval.Progress != "" {
			objectives.AppendProgress(ctx, db, r.ObjectiveID, "[eval] "+eval.Progress)
		}

		switch eval.ObjectiveStatus {
		case "completed":
			objectives.Complete(ctx, db, r.ObjectiveID)
			logger.Log.Infof("[objective-callback] objective #%d completed: %s", r.ObjectiveID, obj.Summary)
		case "continue":
			objectives.UpdateStatus(ctx, db, r.ObjectiveID, "active")
			if eval.FollowUp != "" {
				logger.Log.Infof("[objective-callback] objective #%d needs follow-up: %s", r.ObjectiveID, eval.FollowUp)
				// Conservative: don't chain automatically, just record the follow-up.
				// The next objective pursuit cycle will pick it up.
				objectives.AppendProgress(ctx, db, r.ObjectiveID, "[next] "+eval.FollowUp)
			}
		case "stuck":
			// Leave as in_progress; log for visibility.
			logger.Log.Infof("[objective-callback] objective #%d stuck after attempt %d", r.ObjectiveID, obj.Attempts)
		}
	}
}

// shareEval is the grammar-constrained output from the share quality gate.
type shareEval struct {
	Share   bool   `json:"share"`
	Summary string `json:"summary"`
}

// buildShareGate returns a ShareGate callback that rate-limits and quality-gates
// proactive sharing of background task results with the user.
func buildShareGate(sc *clients.SubagentClient, eng *engine.Engine, maxDaily int) func(directive, result string) {
	if sc == nil || eng == nil || eng.SendFunc == nil {
		return nil
	}
	limiter := engine.NewShareLimiter(maxDaily)
	return func(directive, result string) {
		if !limiter.Allow() {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeouts.SubagentCall)
		defer cancel()

		systemPrompt := `Decide if this background task result is interesting enough to proactively share with the user.
Output JSON: {"share": true/false, "summary": "one-line finding if sharing"}
Rules:
- Only share genuinely useful, surprising, or actionable findings.
- Do NOT share routine/empty results, failures, or things the user already knows.
- Bias toward NOT sharing — only share if it would genuinely help the user.`

		userContent := fmt.Sprintf("Task: %s\nResult:\n%s", directive, textutil.Truncate(result, 1000))

		grammarStr := `root ::= "{" ws "\"share\":" ws boolean "," ws "\"summary\":" ws string ws "}"
boolean ::= "true" | "false"
string ::= "\"" chars "\""
chars ::= char*
char ::= [^"\\] | "\\" escape
escape ::= ["\\nrt/]
ws ::= [ \t\n]*`

		raw, err := sc.TryCompleteWithGrammar(ctx, systemPrompt, userContent, grammarStr, tokens.ShareGate)
		if err != nil {
			return
		}

		var eval shareEval
		if err := json.Unmarshal([]byte(textutil.CleanLLMJSON(raw)), &eval); err != nil || !eval.Share {
			return
		}

		if eval.Summary != "" {
			eng.SendFunc(eval.Summary)
			logger.Log.Infof("[share-gate] proactively shared: %s", eval.Summary)
		}
	}
}
