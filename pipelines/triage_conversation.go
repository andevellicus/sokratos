package pipelines

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/adaptive"
	"sokratos/clients"
	"sokratos/google"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timeouts"
	"sokratos/tokens"
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

// triageViaDTC sends content to the deep thinker (thinking disabled)
// with a GBNF grammar constraint and parses the result into a triageResult.
// In two-model mode, the DTC client points at the Brain (122B).
// Falls back to safe defaults on parse failure.
func triageViaDTC(ctx context.Context, dtc *clients.DeepThinkerClient, triageGrammar, systemPrompt, content string, maxLen int) (*triageResult, error) {
	if len(content) > maxLen {
		content = textutil.Truncate(content, maxLen)
	}

	raw, err := dtc.CompleteNoThinkWithGrammar(ctx, systemPrompt, content, triageGrammar, tokens.TriageDTC)
	if err != nil {
		return nil, fmt.Errorf("triage request: %w", err)
	}

	var result triageResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		logger.Log.Warnf("[triage] parse failure, using fallback: %v (raw: %s)", err, raw)
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
	DTC                *clients.DeepThinkerClient // Brain (122B) in two-model mode; used for triage + paradigm shift
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
	PipelineID    int64      // Telegram message ID of the originating pipeline; 0 = no tracking
	MaxTriageLen  int        // max chars for triage input (typically 8000)
	ShouldSave    func(result *triageResult) bool
}

// triageAndSave is the core triage-then-save pipeline used by both async
// and retry paths for conversation and email triage. It triages via DTC
// (Qwen3.5-27B, no thinking), checks the domain-specific ShouldSave predicate,
// builds and saves the memory, and optionally triggers paradigm shift detection.
func triageAndSave(ctx context.Context, cfg TriageConfig, req TriageSaveRequest) error {
	if cfg.DTC == nil || cfg.TriageGrammar == "" {
		return fmt.Errorf("triage not configured")
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
		PipelineID:    req.PipelineID,
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

	if hasPreferenceTags(tags) && cfg.DTC != nil && cfg.Pool != nil {
		go applyPreferenceFastPath(cfg, result.Summary, req.DomainTag)
	}

	if result.ParadigmShift && result.SalienceScore >= 9 && cfg.DTC != nil {
		go func() {
			psCtx, psCancel := context.WithTimeout(context.Background(), timeouts.ParadigmShift)
			defer psCancel()
			// Build PipelineDeps from TriageConfig.
			psDeps := PipelineDeps{
				Pool: cfg.Pool, DTC: cfg.DTC,
				EmbedEndpoint: cfg.EmbedEndpoint, EmbedModel: cfg.EmbedModel,
				GrammarFn: cfg.BgGrammarFn,
			}
			// 1. Synchronous transition memory generation.
			if _, err := generateTransitionMemory(psCtx, psDeps, result.Summary, tags); err != nil {
				logger.Log.Warnf("[triage:%s] paradigm shift transition failed: %v", req.DomainTag, err)
				return
			}
			// 2. Immediate mini-consolidation to update profile.
			ConsolidateImmediate(psCtx, psDeps)
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
// pipelineID tags saved memories with the originating Telegram message ID for
// prefetch isolation. Runs as a fire-and-forget goroutine.
func TriageAndSaveConversationAsync(cfg TriageConfig, exchange string, toolsUsed bool, pipelineID int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
		defer cancel()

		stripped := strings.TrimSpace(exchange)
		if len(stripped) < 20 {
			logger.Log.Debugf("[conversation_triage] skipped trivial exchange (%d chars)", len(stripped))
			return
		}

		triageInput := truncateAssistantReply(exchange, 800)

		threshold := adaptive.Get(ctx, cfg.Pool, "triage_conversation_threshold", 3.0)
		if !toolsUsed {
			threshold = adaptive.Get(ctx, cfg.Pool, "triage_conversation_unverified_threshold", 5.0)
		}

		err := triageAndSave(ctx, cfg, TriageSaveRequest{
			TriagePrompt:  strings.TrimSpace(prompts.ConversationTriage),
			TriageInput:   triageInput,
			SourceContent: exchange,
			SourceLabel:   "Source exchange",
			DomainTag:     "conversation",
			MemoryType:    "general",
			Source:        "conversation",
			PipelineID:    pipelineID,
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
			} else {
				memory.LogFailedOp(cfg.Pool, "conversation_triage", "triage", err, nil)
			}
		}
	}()
}

// TriageAndSaveEmailAsync sends an email for triage scoring, then saves it to
// memory if it meets the salience threshold. Runs as a fire-and-forget goroutine.
func TriageAndSaveEmailAsync(cfg TriageConfig, email google.Email) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
		defer cancel()

		// Inject user preferences into the triage prompt so the model can
		// filter emails about topics the user has expressed disinterest in.
		triagePrompt := strings.TrimSpace(prompts.EmailTriage)
		if prefs := memory.FormatPreferencesForTriage(ctx, cfg.Pool); prefs != "" {
			triagePrompt += "\n\n" + prefs
		}

		formatted := google.FormatEmailSummary(email)
		sourceDate := email.Date

		err := triageAndSave(ctx, cfg, TriageSaveRequest{
			TriagePrompt:  triagePrompt,
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
				return r.SalienceScore >= adaptive.Get(ctx, cfg.Pool, "triage_email_threshold", 1.0)
			},
		})
		if err != nil {
			logger.Log.Warnf("[email_triage] failed: %v", err)
			if cfg.RetryQueue != nil {
				EnqueueEmailTriage(cfg.RetryQueue, cfg, formatted, formatted)
			} else {
				memory.LogFailedOp(cfg.Pool, "email_triage", "triage", err, nil)
			}
		}
	}()
}

// preferenceTagSet is the set of triage tags that indicate user preference/behavior feedback.
var preferenceTagSet = map[string]struct{}{
	"preferences":         {},
	"communication_style": {},
	"behavior":            {},
	"response_style":      {},
}

// hasPreferenceTags returns true if any of the tags match a known preference indicator.
func hasPreferenceTags(tags []string) bool {
	for _, t := range tags {
		if _, ok := preferenceTagSet[t]; ok {
			return true
		}
	}
	return false
}

// preferenceExtractionGrammar constrains the DTC output to a valid preference trait.
const preferenceExtractionGrammar = `root ::= "{" ws "\"category\":" ws cat "," ws "\"key\":" ws string "," ws "\"value\":" ws string "," ws "\"context\":" ws string ws "}"
cat ::= "\"style\"" | "\"preference\""
string ::= "\"" [^"\\]* "\""
ws ::= [ \t\n\r]*`

// applyPreferenceFastPath extracts a personality trait from a preference-tagged
// triage summary and upserts it into personality_traits. Mirrors the paradigm
// shift fast-path pattern.
func applyPreferenceFastPath(cfg TriageConfig, summary, domainTag string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.MemorySave)
	defer cancel()

	prompt := `Extract one personality trait from this user preference. Return JSON:
{"category": "style" or "preference", "key": "snake_case_id", "value": "the preference", "context": "original statement"}

- "style" for communication preferences (verbosity, tone, formality, content filtering)
- "preference" for general preferences (naming, scheduling, workflow)
- key: brief snake_case identifier (e.g. "hobby_references", "verbosity", "emoji_usage")
- value: concise description of what the user wants`

	raw, err := cfg.DTC.CompleteNoThinkWithGrammar(ctx, prompt, summary, preferenceExtractionGrammar, tokens.PreferenceExtract)
	if err != nil {
		logger.Log.Warnf("[triage:%s] preference extraction failed: %v", domainTag, err)
		return
	}

	var trait struct {
		Category string `json:"category"`
		Key      string `json:"key"`
		Value    string `json:"value"`
		Context  string `json:"context"`
	}
	if err := json.Unmarshal([]byte(raw), &trait); err != nil {
		logger.Log.Warnf("[triage:%s] preference parse failed: %v (raw: %s)", domainTag, err, textutil.Truncate(raw, 200))
		return
	}

	// Only accept style/preference categories — reject hallucinated ones.
	if trait.Category != "style" && trait.Category != "preference" {
		logger.Log.Warnf("[triage:%s] preference extraction returned invalid category %q, skipping", domainTag, trait.Category)
		return
	}
	if trait.Key == "" || trait.Value == "" {
		logger.Log.Warnf("[triage:%s] preference extraction returned empty key/value, skipping", domainTag)
		return
	}

	if _, err := memory.UpsertPersonalityTrait(ctx, cfg.Pool, trait.Category, trait.Key, trait.Value, trait.Context); err != nil {
		logger.Log.Warnf("[triage:%s] preference upsert failed: %v", domainTag, err)
		return
	}

	if cfg.ProfileRefreshFunc != nil {
		cfg.ProfileRefreshFunc()
	}
	logger.Log.Infof("[triage:%s] preference fast-path applied %s/%s: %s", domainTag, trait.Category, trait.Key, trait.Value)
}
