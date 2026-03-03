package pipelines

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/clients"
	"sokratos/gmail"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
)

// triageResult is the structured output from a triage call.
type triageResult struct {
	SalienceScore float64  `json:"salience_score"`
	Summary       string   `json:"summary"`
	Category      string   `json:"category"`
	Tags          []string `json:"tags"`
	Save          *bool    `json:"save,omitempty"`
	ParadigmShift bool     `json:"paradigm_shift,omitempty"`
}

// triageViaDTC sends content to the deep thinker (Qwen3.5-27B, no thinking)
// with a GBNF grammar constraint and parses the result into a triageResult.
// Falls back to safe defaults on parse failure.
func triageViaDTC(ctx context.Context, dtc *clients.DeepThinkerClient, triageGrammar, systemPrompt, content string, maxLen int) (*triageResult, error) {
	if len(content) > maxLen {
		content = textutil.Truncate(content, maxLen)
	}

	raw, err := dtc.CompleteNoThinkWithGrammar(ctx, systemPrompt, content, triageGrammar, 2048)
	if err != nil {
		return nil, fmt.Errorf("dtc triage request: %w", err)
	}

	var result triageResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		logger.Log.Warnf("[conversation_triage] dtc parse failure, using fallback: %v (raw: %s)", err, raw)
		summary := textutil.Truncate(content, 200)
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

	// First unlink any superseded_by references pointing at the rows
	// we're about to delete, then delete. Without this, the FK
	// constraint on superseded_by prevents deletion.
	_, _ = pool.Exec(ctx,
		`UPDATE memories SET superseded_by = NULL
		 WHERE superseded_by IN (
		   SELECT id FROM memories
		   WHERE 'conversation' = ANY(tags)
		     AND summary NOT LIKE '%Triage:%'
		 )`)
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

// truncateAssistantReply caps the assistant portion of a "user: ...\nassistant: ..."
// exchange so the user's statement dominates the triage input. Returns the
// original exchange unchanged if no assistant prefix is found or if the reply
// is already short enough.
func truncateAssistantReply(exchange string, maxReplyLen int) string {
	idx := strings.Index(exchange, "\nassistant: ")
	if idx < 0 {
		return exchange
	}
	replyStart := idx + len("\nassistant: ")
	reply := exchange[replyStart:]
	if len(reply) <= maxReplyLen {
		return exchange
	}
	return exchange[:replyStart] + textutil.Truncate(reply, maxReplyLen)
}

// TriageConfig groups dependencies for conversation triage and memory save.
type TriageConfig struct {
	Pool               *pgxpool.Pool
	EmbedEndpoint      string
	EmbedModel         string
	DTC                *clients.DeepThinkerClient
	QueueFn            memory.WorkQueueFunc       // background work queue for quality enrichment + deferred work
	BgGrammarFn        memory.GrammarSubagentFunc // non-blocking variant for contradiction checks + entity extraction
	TriageGrammar      string
	RetryQueue         *RetryQueue // deferred triage retry queue (nil = drop on failure)
	ProfileRefreshFunc func()      // called after paradigm shift to refresh engine profile + personality
}

// TriageSaveRequest encapsulates all parameters for a triage-then-save operation.
// Domain-specific threshold logic lives in the ShouldSave closure.
type TriageSaveRequest struct {
	TriagePrompt  string     // system prompt for the triage LLM
	TriageInput   string     // content to triage (may be truncated by caller)
	SourceContent string     // full source content for storage (capped at 2000 chars internally)
	SourceLabel   string     // "Source exchange" or "Source email"
	DomainTag     string     // "conversation" or "email" — prepended to tags
	MemoryType    string     // "general" or "email"
	Source        string     // "conversation" or "email"
	SourceDate    *time.Time // optional: for email source dates
	MaxTriageLen  int        // max chars for triage input (typically 8000)
	ShouldSave    func(result *triageResult) bool
}

// triageAndSave is the core triage-then-save pipeline used by both async
// and retry paths for conversation and email triage. It triages via DTC
// (Qwen3.5-27B, no thinking), checks the domain-specific ShouldSave predicate,
// builds and saves the memory, and optionally triggers paradigm shift detection.
func triageAndSave(ctx context.Context, cfg TriageConfig, req TriageSaveRequest) error {
	if cfg.DTC == nil || cfg.TriageGrammar == "" {
		return fmt.Errorf("DTC not configured for triage")
	}

	// Context-aware triage: check if similar memories already exist.
	// If coverage is high, annotate the triage input to raise the bar.
	triageInput := req.TriageInput
	if cfg.EmbedEndpoint != "" && cfg.Pool != nil {
		snippet := triageInput
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		emb, embErr := memory.GetEmbedding(ctx, cfg.EmbedEndpoint, cfg.EmbedModel, snippet)
		if embErr == nil {
			var count int
			_ = cfg.Pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM memories
				 WHERE superseded_by IS NULL
				   AND (embedding <=> $1) < 0.3
				   AND memory_type NOT IN (`+memory.FormatSQLExclusion(memory.ExcludeInternal)+`)`,
				pgvector.NewVector(emb),
			).Scan(&count)
			if count >= 3 {
				triageInput += fmt.Sprintf("\n[Memory coverage: %d similar memories exist. Only save if genuinely NEW information.]", count)
			}
		}
	}

	result, err := triageViaDTC(ctx, cfg.DTC, cfg.TriageGrammar, req.TriagePrompt, triageInput, req.MaxTriageLen)
	if err != nil {
		return err
	}

	if !req.ShouldSave(result) {
		logger.Log.Infof("[triage:%s] skipped (score=%.0f): %s", req.DomainTag, result.SalienceScore, result.Summary)
		return nil
	}

	// Store only the triage summary as the memory text. The source exchange
	// is intentionally excluded: the embedding model's 512-token window is
	// better served by a clean summary, and appending raw exchanges caused
	// junk chunk-2 fragments and assistant-generated content leaking into
	// memory. Contradiction detection already strips source exchanges.
	text := result.Summary
	tags := append([]string{req.DomainTag}, result.Tags...)

	memReq := memory.MemoryWriteRequest{
		Summary:       text,
		Tags:          tags,
		Salience:      result.SalienceScore,
		MemoryType:    req.MemoryType,
		Source:        req.Source,
		SourceDate:    req.SourceDate,
		EmbedEndpoint: cfg.EmbedEndpoint,
		EmbedModel:    cfg.EmbedModel,
	}
	if cfg.BgGrammarFn != nil {
		if _, saveErr := memory.CheckAndWriteWithContradiction(ctx, cfg.Pool, memReq, cfg.BgGrammarFn, cfg.QueueFn); saveErr != nil {
			logger.Log.Warnf("[triage:%s] save failed: %v", req.DomainTag, saveErr)
			return nil // save failed but triage succeeded — don't retry
		}
	} else {
		memory.SaveToMemoryWithSalienceAsync(cfg.Pool, memReq, nil, cfg.QueueFn)
	}

	logger.Log.Infof("[triage:%s] saved (score=%.0f, category=%s, tags=%v): %s",
		req.DomainTag, result.SalienceScore, result.Category, tags, result.Summary)

	if result.ParadigmShift && result.SalienceScore >= 9 && cfg.DTC != nil {
		go func() {
			psCtx, psCancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer psCancel()
			// 1. Synchronous transition memory generation.
			if _, err := generateTransitionMemory(psCtx, cfg.Pool, cfg.EmbedEndpoint, cfg.EmbedModel, cfg.DTC, result.Summary, tags); err != nil {
				logger.Log.Warnf("[triage:%s] paradigm shift transition failed: %v", req.DomainTag, err)
				return
			}
			// 2. Immediate mini-consolidation to update profile.
			ConsolidateImmediate(psCtx, cfg.Pool, cfg.DTC, cfg.EmbedEndpoint, cfg.EmbedModel, cfg.BgGrammarFn)
			// 3. Refresh engine state so updated profile is used immediately.
			if cfg.ProfileRefreshFunc != nil {
				cfg.ProfileRefreshFunc()
			}
		}()
	}
	return nil
}

// TriageAndSaveConversationAsync sends a conversation exchange for triage
// scoring, then saves it to memory if it meets the salience threshold.
// When toolsUsed is true (exchange was grounded by tool results), the threshold
// is 3. When false (pure parametric knowledge), the threshold is raised to 5
// to prevent hallucinated facts from entering memory and creating feedback loops.
// Runs as a fire-and-forget goroutine.
func TriageAndSaveConversationAsync(cfg TriageConfig, exchange string, toolsUsed bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
		defer cancel()

		stripped := strings.TrimSpace(exchange)
		if len(stripped) < 20 {
			logger.Log.Debugf("[conversation_triage] skipped trivial exchange (%d chars)", len(stripped))
			return
		}

		triageInput := truncateAssistantReply(exchange, 800)

		threshold := float64(3)
		if !toolsUsed {
			threshold = 5
		}

		err := triageAndSave(ctx, cfg, TriageSaveRequest{
			TriagePrompt:  strings.TrimSpace(prompts.ConversationTriage),
			TriageInput:   triageInput,
			SourceContent: exchange,
			SourceLabel:   "Source exchange",
			DomainTag:     "conversation",
			MemoryType:    "general",
			Source:        "conversation",
			MaxTriageLen:  8000,
			ShouldSave: func(r *triageResult) bool {
				if r.Save != nil {
					logger.Log.Debugf("[conversation_triage] save=%v (score=%.0f)", *r.Save, r.SalienceScore)
				}
				if r.SalienceScore < threshold {
					if !toolsUsed && r.SalienceScore >= 3 {
						logger.Log.Infof("[conversation_triage] skipped (unverified, score=%.0f): %s", r.SalienceScore, r.Summary)
					}
					return false
				}
				return true
			},
		})
		if err != nil {
			logger.Log.Warnf("[conversation_triage] triage failed: %v", err)
			if cfg.RetryQueue != nil {
				EnqueueConversationTriage(cfg.RetryQueue, cfg, triageInput, exchange, toolsUsed)
			}
		}
	}()
}

// TriageAndSaveEmailAsync sends an email for triage scoring, then saves it to
// memory if it meets the salience threshold. Runs as a fire-and-forget goroutine.
func TriageAndSaveEmailAsync(cfg TriageConfig, email gmail.Email) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
		defer cancel()

		formatted := gmail.FormatEmailSummary(email)
		sourceDate := email.Date

		err := triageAndSave(ctx, cfg, TriageSaveRequest{
			TriagePrompt:  strings.TrimSpace(prompts.EmailTriage),
			TriageInput:   formatted,
			SourceContent: formatted,
			SourceLabel:   "Source email",
			DomainTag:     "email",
			MemoryType:    "email",
			Source:        "email",
			SourceDate:    &sourceDate,
			MaxTriageLen:  8000,
			ShouldSave: func(r *triageResult) bool {
				if r.Save != nil && !*r.Save {
					return false
				}
				return r.SalienceScore >= 1
			},
		})
		if err != nil {
			logger.Log.Warnf("[email_triage] failed: %v", err)
			if cfg.RetryQueue != nil {
				EnqueueEmailTriage(cfg.RetryQueue, cfg, formatted, formatted)
			}
		}
	}()
}
