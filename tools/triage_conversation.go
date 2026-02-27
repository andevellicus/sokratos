package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
)

// TriageViaSubagent sends content to the subagent with a GBNF-constrained
// grammar and parses the result into a triageResult. Falls back to safe
// defaults on parse failure.
func TriageViaSubagent(ctx context.Context, sc *SubagentClient, triageGrammar, systemPrompt, content string, maxLen int) (*triageResult, error) {
	if len(content) > maxLen {
		content = content[:maxLen] + "..."
	}

	raw, err := sc.CompleteWithGrammar(ctx, systemPrompt, content, triageGrammar, 2048)
	if err != nil {
		return nil, fmt.Errorf("subagent triage request: %w", err)
	}

	var result triageResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		logger.Log.Warnf("[conversation_triage] subagent parse failure, using fallback: %v (raw: %s)", err, raw)
		summary := content
		if len(summary) > 200 {
			summary = summary[:200] + "..."
		}
		return &triageResult{
			SalienceScore: 5,
			Summary:       summary,
			Tags:          nil,
		}, nil
	}
	return &result, nil
}

// CleanupPreTriageMemories deletes conversation-tagged memories that were
// saved before the triage system was introduced (identified by lacking
// "Triage:" metadata in their text). These blind saves include "I don't know"
// responses that poison search results.
func CleanupPreTriageMemories(pool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutMemorySave)
	defer cancel()

	result, err := pool.Exec(ctx,
		`DELETE FROM memories
		 WHERE 'conversation' = ANY(tags)
		   AND summary NOT LIKE '%Triage:%'`)
	if err != nil {
		logger.Log.Errorf("[cleanup] failed to delete pre-triage conversation memories: %v", err)
		return
	}
	logger.Log.Infof("[cleanup] deleted %d pre-triage conversation memories", result.RowsAffected())
}

// TriageConfig groups dependencies for conversation triage and memory save.
type TriageConfig struct {
	Pool          *pgxpool.Pool
	EmbedEndpoint string
	EmbedModel    string
	DTC           *DeepThinkerClient
	SubagentFn    memory.SubagentFunc
	Subagent      *SubagentClient
	TriageGrammar string
}

// TriageAndSaveConversationAsync sends a conversation exchange for triage
// scoring, then saves it to memory if it meets the salience threshold.
// When toolsUsed is true (exchange was grounded by tool results), the threshold
// is 3. When false (pure parametric knowledge), the threshold is raised to 5
// to prevent hallucinated facts from entering memory and creating feedback loops.
// When Subagent is non-nil, routes through the subagent with GBNF grammar;
// otherwise falls back to the deep thinker. When SubagentFn is non-nil,
// contradiction detection and quality scoring are applied on save.
// Runs as a fire-and-forget goroutine.
func TriageAndSaveConversationAsync(cfg TriageConfig, exchange string, toolsUsed bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
		defer cancel()

		triagePrompt := strings.TrimSpace(prompts.ConversationTriage)

		var result *triageResult
		var err error
		if cfg.Subagent != nil && cfg.TriageGrammar != "" {
			result, err = TriageViaSubagent(ctx, cfg.Subagent, cfg.TriageGrammar, triagePrompt, exchange, 4000)
		} else {
			result, err = cfg.DTC.TriageItem(ctx, triagePrompt, exchange, 4000)
		}
		if err != nil {
			logger.Log.Warnf("[conversation_triage] triage failed: %v", err)
			return
		}

		if result.Save != nil {
			logger.Log.Debugf("[conversation_triage] save=%v for exchange (score=%.0f)", *result.Save, result.SalienceScore)
		}

		threshold := float64(3)
		if !toolsUsed {
			threshold = 5
		}
		if result.SalienceScore < threshold {
			if !toolsUsed && result.SalienceScore >= 3 {
				logger.Log.Infof("[conversation_triage] skipped (unverified, score=%.0f): %s", result.SalienceScore, result.Summary)
			} else {
				logger.Log.Infof("[conversation_triage] skipped (score=%.0f): %s", result.SalienceScore, result.Summary)
			}
			return
		}

		// Put the clean summary first so it dominates the embedding model's
		// 512-token window, with the full exchange preserved as context.
		text := fmt.Sprintf("%s\n\nSource exchange:\n%s", result.Summary, exchange)
		tags := append([]string{"conversation"}, result.Tags...)

		req := memory.MemoryWriteRequest{
			Summary:       text,
			Tags:          tags,
			Salience:      result.SalienceScore,
			MemoryType:    "general",
			Source:        "conversation",
			EmbedEndpoint: cfg.EmbedEndpoint,
			EmbedModel:    cfg.EmbedModel,
		}
		if cfg.SubagentFn != nil {
			if _, err := memory.CheckAndWriteWithContradiction(ctx, cfg.Pool, req, cfg.SubagentFn); err != nil {
				logger.Log.Warnf("[conversation_triage] contradiction-checked save failed: %v", err)
				return
			}
		} else {
			memory.SaveToMemoryWithSalienceAsync(cfg.Pool, req, nil)
		}

		logger.Log.Infof("[conversation_triage] saved (score=%.0f, category=%s, tags=%v): %s", result.SalienceScore, result.Category, tags, result.Summary)

		if result.ParadigmShift && result.SalienceScore >= 9 && cfg.DTC != nil {
			GenerateTransitionMemoryAsync(cfg.Pool, cfg.EmbedEndpoint, cfg.EmbedModel, cfg.DTC, result.Summary, tags)
		}
	}()
}
