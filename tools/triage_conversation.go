package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
)

// GraniteTriageConfig holds the dependencies for routing conversation triage
// through the Granite tool agent with GBNF grammar constraints.
type GraniteTriageConfig struct {
	Client  *llm.Client
	Model   string
	Grammar string
}

// TriageViaGranite sends content to Granite with a GBNF-constrained grammar
// and parses the result into a triageResult. Falls back to safe defaults on
// parse failure (Granite should always produce valid JSON via grammar, but
// defensive coding is cheap).
func TriageViaGranite(ctx context.Context, cfg *GraniteTriageConfig, systemPrompt, content string, maxLen int) (*triageResult, error) {
	if len(content) > maxLen {
		content = content[:maxLen] + "..."
	}

	resp, err := cfg.Client.Chat(ctx, llm.ChatRequest{
		Model: cfg.Model,
		Messages: []llm.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: content},
		},
		Grammar: cfg.Grammar,
	})
	if err != nil {
		return nil, fmt.Errorf("granite triage request: %w", err)
	}

	raw := strings.TrimSpace(resp.Message.Content)
	var result triageResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		logger.Log.Warnf("[conversation_triage] granite parse failure, using fallback: %v (raw: %s)", err, raw)
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

// TriageAndSaveConversationAsync sends a conversation exchange for triage
// scoring, then saves it to memory if it contains new information (salience >= 3).
// When graniteTriage is non-nil, routes through Granite with GBNF grammar;
// otherwise falls back to the deep thinker. When granite (GraniteFunc) is
// non-nil, contradiction detection and quality scoring are applied on save.
// Runs as a fire-and-forget goroutine.
func TriageAndSaveConversationAsync(pool *pgxpool.Pool, embedEndpoint, embedModel string, dtc *DeepThinkerClient, granite memory.GraniteFunc, graniteTriage *GraniteTriageConfig, exchange string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
		defer cancel()

		triagePrompt := strings.TrimSpace(prompts.ConversationTriage)

		var result *triageResult
		var err error
		if graniteTriage != nil {
			result, err = TriageViaGranite(ctx, graniteTriage, triagePrompt, exchange, 4000)
		} else {
			result, err = dtc.TriageItem(ctx, triagePrompt, exchange, 4000)
		}
		if err != nil {
			logger.Log.Warnf("[conversation_triage] triage failed: %v", err)
			return
		}

		if result.Save != nil {
			logger.Log.Debugf("[conversation_triage] save=%v for exchange (score=%.0f)", *result.Save, result.SalienceScore)
		}

		if result.SalienceScore < 3 {
			logger.Log.Infof("[conversation_triage] skipped (score=%.0f): %s", result.SalienceScore, result.Summary)
			return
		}

		// Put the clean summary first so it dominates the embedding model's
		// 512-token window, with the full exchange preserved as context.
		text := fmt.Sprintf("%s\n\nSource exchange:\n%s", result.Summary, exchange)
		tags := append([]string{"conversation"}, result.Tags...)

		if granite != nil {
			req := memory.MemoryWriteRequest{
				Summary:       text,
				Tags:          tags,
				Salience:      result.SalienceScore,
				MemoryType:    "general",
				Source:        "conversation",
				EmbedEndpoint: embedEndpoint,
				EmbedModel:    embedModel,
			}
			if _, err := memory.CheckAndWriteWithContradiction(ctx, pool, req, granite); err != nil {
				logger.Log.Warnf("[conversation_triage] contradiction-checked save failed: %v", err)
				return
			}
		} else {
			memory.SaveToMemoryWithSalienceAsync(pool, embedEndpoint, embedModel, text, result.SalienceScore, tags, "conversation", nil)
		}

		logger.Log.Infof("[conversation_triage] saved (score=%.0f, category=%s, tags=%v): %s", result.SalienceScore, result.Category, tags, result.Summary)
	}()
}
