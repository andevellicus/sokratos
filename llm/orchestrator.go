package llm

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"sokratos/orchestrate"
	"sokratos/prompts"
)

const orchestratorMaxTokens = 4096

// buildOrchestratorSystemPrompt constructs the system prompt for the
// grammar-constrained orchestrator. Identity card and personality are injected
// between the core rules and tool descriptions.
func buildOrchestratorSystemPrompt(toolDescs, profileContent, personalityContent string) string {
	sp := systemPromptBase

	// Identity card goes right after the core rules — before tools and
	// everything else — so even small models pay attention to it.
	if profileContent != "" {
		sp += "\n\n## Identity Card\n" + profileContent
	}
	if personalityContent != "" {
		sp += "\n\n" + personalityContent
	}

	// Tool descriptions follow after identity/personality.
	sp += "\n\n" + strings.TrimSpace(toolDescs)

	return sp
}

// runGrammarOrchestrator builds the system prompt and delegates to the unified
// orchestrate.RunLoop. This is a thin adapter that translates llm-specific
// types (Client, Message, QueryOrchestratorOpts) into orchestrate types.
func runGrammarOrchestrator(ctx context.Context, client *Client, model, grammarStr, prompt string,
	toolExec func(context.Context, json.RawMessage) (string, error),
	trimFn func([]Message) []Message, opts *QueryOrchestratorOpts) (string, []Message, error) {

	// Build system prompt.
	toolDescs := prompts.Tools
	if opts != nil && opts.ToolAgent != nil && opts.ToolAgent.ToolDescriptions != "" {
		toolDescs = opts.ToolAgent.ToolDescriptions
	}
	var profileContent, personalityContent string
	if opts != nil {
		profileContent = opts.ProfileContent
		personalityContent = opts.PersonalityContent
	}
	sysContent := buildOrchestratorSystemPrompt(toolDescs, profileContent, personalityContent)

	// Replace the %MAX_WEB_SOURCES% placeholder.
	maxWeb := "2"
	if opts != nil && opts.MaxWebSources > 0 {
		maxWeb = strconv.Itoa(opts.MaxWebSources)
	}
	sysContent = strings.Replace(sysContent, "%MAX_WEB_SOURCES%", maxWeb, 1)
	if opts != nil && opts.TemporalContext != "" {
		sysContent += "\n\n" + opts.TemporalContext
	}
	if opts != nil && opts.PrefetchContent != "" {
		sysContent += "\n\n" + opts.PrefetchContent
	}

	// Build initial messages.
	messages := []orchestrate.Message{{Role: "system", Content: sysContent}}
	if opts != nil && len(opts.History) > 0 {
		for _, m := range opts.History {
			messages = append(messages, orchestrate.Message{Role: m.Role, Content: m.Content})
		}
	}
	historyLen := len(messages)

	userMsg := orchestrate.Message{Role: "user", Content: prompt}
	messages = append(messages, userMsg)

	// Wrap llm.Client.Chat as orchestrate.ChatFunc.
	chatFn := func(ctx context.Context, req orchestrate.ChatInput) (string, error) {
		llmMsgs := make([]Message, len(req.Messages))
		for i, m := range req.Messages {
			llmMsgs[i] = Message{Role: m.Role, Content: m.Content}
		}
		// Preserve vision parts on the user message if provided.
		if opts != nil && len(opts.Parts) > 0 {
			// Find the original user message (after system + history).
			if historyLen < len(llmMsgs) {
				llmMsgs[historyLen].Parts = opts.Parts
			}
		}
		resp, err := client.Chat(ctx, ChatRequest{
			Model:              model,
			Messages:           llmMsgs,
			Grammar:            req.Grammar,
			MaxTokens:          req.MaxTokens,
			ReasoningFormat:    req.ReasoningFormat,
			ChatTemplateKwargs: req.ChatTemplateKwargs,
		})
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(resp.Message.Content), nil
	}

	// Build LoopConfig from opts.
	cfg := orchestrate.LoopConfig{
		Grammar:            grammarStr,
		MaxTokens:          orchestratorMaxTokens,
		InjectTimestamp:     true,
		SoftErrorDetection: true,
		UserGoal:           prompt,
	}
	if opts != nil {
		cfg.Fallbacks = opts.Fallbacks
		cfg.MandatedBrainTools = opts.MandatedBrainTools
		cfg.EnableThinking = opts.EnableThinking
		cfg.FirstRoundThinking = opts.FirstRoundThinking
		cfg.OnToolStart = opts.OnToolStart
		cfg.OnToolEnd = opts.OnToolEnd
		cfg.OnToolExec = opts.OnToolExec
		if opts.MaxToolResultLen > 0 {
			cfg.MaxToolResultLen = opts.MaxToolResultLen
		}
	}

	// Wrap trimFn to convert between message types.
	if trimFn != nil {
		cfg.TrimFn = func(msgs []orchestrate.Message) []orchestrate.Message {
			llmMsgs := make([]Message, len(msgs))
			for i, m := range msgs {
				llmMsgs[i] = Message{Role: m.Role, Content: m.Content}
			}
			trimmed := trimFn(llmMsgs)
			result := make([]orchestrate.Message, len(trimmed))
			for i, m := range trimmed {
				result[i] = orchestrate.Message{Role: m.Role, Content: m.Content}
			}
			return result
		}
	}

	response, newMsgs, err := orchestrate.RunLoop(ctx, chatFn, messages, toolExec, cfg)

	// Convert back to llm.Message and return only new messages (after history).
	var llmNewMsgs []Message
	if len(newMsgs) > historyLen {
		for _, m := range newMsgs[historyLen:] {
			llmNewMsgs = append(llmNewMsgs, Message{Role: m.Role, Content: m.Content})
		}
	}
	return response, llmNewMsgs, err
}
