package orchestrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sokratos/logger"
	"sokratos/textutil"
	"sokratos/timefmt"
)

const (
	defaultMaxRounds       = 15
	defaultMaxTokens       = 4096
	defaultMaxToolResult   = 8000
	defaultOverflowHint    = "Use specific queries or filters to narrow results"
	maxParseRetries        = 3
	maxToolErrorRetries    = 3
)

// decision is the grammar-constrained JSON the LLM produces.
type decision struct {
	Action    string          `json:"action"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Text      string          `json:"text,omitempty"`
}

// RunLoop executes the unified grammar-constrained orchestrator/supervisor loop.
// The LLM is called via chatFn; tool calls are executed via toolExec.
// Returns the final response text, updated messages (including new rounds), and error.
func RunLoop(ctx context.Context, chatFn ChatFunc, messages []Message,
	toolExec ToolExecFunc, cfg LoopConfig) (string, []Message, error) {

	maxRounds := cfg.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultMaxRounds
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	maxToolResult := cfg.MaxToolResultLen
	if maxToolResult <= 0 {
		maxToolResult = defaultMaxToolResult
	}
	overflowHint := cfg.ToolResultOverflowHint
	if overflowHint == "" {
		overflowHint = defaultOverflowHint
	}

	// callTool wraps toolExec with slot release/reacquire callbacks and timing.
	callTool := func(ctx context.Context, toolName string, raw []byte) (string, error) {
		if cfg.OnToolStart != nil {
			cfg.OnToolStart(toolName)
		}
		toolStart := time.Now()
		result, err := toolExec(ctx, raw)
		if cfg.OnToolExec != nil {
			cfg.OnToolExec(toolName, time.Since(toolStart), err)
		}
		if cfg.OnToolEnd != nil {
			if reErr := cfg.OnToolEnd(ctx); reErr != nil {
				logger.Log.Warnf("[orchestrate] slot reacquire failed: %v", reErr)
			}
		}
		return result, err
	}

	parseRetries := 0
	errorRetries := 0

	for round := range maxRounds {
		// Enable thinking for this round based on the mode.
		thinkThisRound := cfg.EnableThinking || (cfg.FirstRoundThinking && round == 0)

		// Prepare grammar and reasoning format for thinking.
		roundGrammar, reasoningFmt := prepareThinking(cfg.Grammar, thinkThisRound)

		if thinkThisRound {
			logger.Log.Debugf("[orchestrate] round %d: thinking enabled (grammar=%v)", round, roundGrammar != "")
		}

		sent := messages
		if cfg.TrimFn != nil {
			sent = cfg.TrimFn(messages)
		}

		// Inject current time into the system prompt if configured.
		if cfg.InjectTimestamp && len(sent) > 0 && sent[0].Role == "system" {
			sent = append([]Message{}, sent...)
			sent[0] = Message{
				Role:    sent[0].Role,
				Content: sent[0].Content + "\n\nCurrent time: " + timefmt.FormatNatural(time.Now()),
			}
		}

		chatReq := ChatInput{
			Messages:           sent,
			Grammar:            roundGrammar,
			MaxTokens:          maxTokens,
			ReasoningFormat:    reasoningFmt,
			ChatTemplateKwargs: map[string]any{"enable_thinking": thinkThisRound},
		}
		raw, cErr := chatFn(ctx, chatReq)
		if cErr != nil {
			return "", messages, cErr
		}

		raw = strings.TrimSpace(raw)
		content, stored, thinking := processThinking(raw)
		if thinking != "" {
			logger.Log.Infof("[orchestrate:thinking] %s", thinking)
		}
		logger.Log.Infof("[orchestrate] %s", textutil.Truncate(content, 300))

		// Parse grammar-constrained JSON.
		var dec decision
		if err := json.Unmarshal([]byte(content), &dec); err != nil {
			cleaned := textutil.CleanLLMJSON(content)
			if err2 := json.Unmarshal([]byte(cleaned), &dec); err2 != nil {
				parseRetries++
				if parseRetries <= maxParseRetries {
					logger.Log.Warnf("[orchestrate] round %d: grammar parse failed, retrying (%d/%d) (raw: %.200s)",
						round, parseRetries, maxParseRetries, content)
					continue
				}
				return "", messages, fmt.Errorf("parse decision round %d: %w (raw: %.200s)", round, err, content)
			}
		}
		parseRetries = 0

		if dec.Action == "respond" {
			messages = append(messages, Message{Role: "assistant", Content: stored})
			logger.Log.Infof("[orchestrate] completed in %d round(s)", round+1)
			return dec.Text, messages, nil
		}

		if dec.Action != "tool" {
			return "", messages, fmt.Errorf("unknown action %q", dec.Action)
		}

		// Tool call path.
		toolName := dec.Name
		toolJSON, _ := json.Marshal(map[string]any{"name": toolName, "arguments": dec.Arguments})
		logger.Log.Infof("[orchestrate] round %d: calling %s", round+1, toolName)

		if cfg.ProgressFn != nil {
			cfg.ProgressFn(fmt.Sprintf("Step %d/%d: calling %s", round+1, maxRounds, toolName))
		}

		messages = append(messages, Message{Role: "assistant", Content: stored})

		if toolExec == nil {
			messages = append(messages, Message{Role: "user", Content: "Tool result: no tool executor configured"})
			continue
		}

		// Intercept mandated brain tools.
		if cfg.MandatedBrainTools != nil {
			if taskType, isMBT := cfg.MandatedBrainTools[toolName]; isMBT {
				return "", messages, &BackgroundJobRequest{
					Tool:     toolName,
					UserGoal: cfg.UserGoal,
					TaskType: taskType,
				}
			}
		}

		// Execute the tool.
		result, execErr := callTool(ctx, toolName, toolJSON)

		// Check for BackgroundJobRequest from tool (e.g. deep_think with background=true).
		var bjr *BackgroundJobRequest
		if errors.As(execErr, &bjr) {
			return "", messages, bjr
		}

		// Determine failure.
		var failureMsg string
		isFailed := false
		if execErr != nil {
			failureMsg = execErr.Error()
			isFailed = true
		} else if cfg.SoftErrorDetection && IsToolSoftError(result) {
			failureMsg = result
			isFailed = true
		}

		// Try deterministic fallback.
		if isFailed {
			if fb, ok := matchFallback(cfg.Fallbacks, toolName, failureMsg); ok {
				fbArgs := fb.ArgsTransform(toolName, dec.Arguments, failureMsg)
				fbJSON := buildToolJSON(fb.FallbackTool, fbArgs)
				logger.Log.Infof("[orchestrate] auto-fallback: %s failed, trying %s", toolName, fb.FallbackTool)
				fbResult, fbErr := callTool(ctx, fb.FallbackTool, []byte(fbJSON))
				if fbErr != nil {
					messages = append(messages, Message{Role: "user", Content: fmt.Sprintf(
						"Tool result [auto-fallback]: %s failed (%s). Fallback to %s also failed: %s",
						toolName, failureMsg, fb.FallbackTool, fbErr.Error())})
				} else {
					fbResult = textutil.TruncateToolResult(fbResult, maxToolResult, "")
					messages = append(messages, Message{Role: "user", Content: fmt.Sprintf(
						"Tool result [auto-fallback]: %s failed (%s). Fallback to %s:\n%s",
						toolName, failureMsg, fb.FallbackTool, fbResult)})
				}
				continue
			}

			// No fallback — inject hint.
			hint := toolHint(toolName)
			if execErr != nil {
				errorRetries++
				if errorRetries <= maxToolErrorRetries {
					messages = append(messages, Message{Role: "user", Content: fmt.Sprintf(
						"Tool error (attempt %d/%d): %s%s\nReformulate with corrected parameters or try a different tool.",
						errorRetries, maxToolErrorRetries, execErr.Error(), hint)})
				} else {
					messages = append(messages, Message{Role: "user", Content: "Tool error (retries exhausted): " + execErr.Error() + hint})
				}
			} else {
				result = textutil.TruncateToolResult(result, maxToolResult, "")
				messages = append(messages, Message{Role: "user", Content: "Tool result: " + result + hint})
			}
			continue
		}

		errorRetries = 0
		result = textutil.TruncateToolResult(result, maxToolResult, overflowHint)
		messages = append(messages, Message{Role: "user", Content: "Tool result: " + result})
	}

	return "", messages, fmt.Errorf("too many tool call rounds")
}
