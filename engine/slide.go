package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
	"sokratos/timefmt"
	"sokratos/timeouts"
)

// ArchiveDeps groups dependencies for context archival (embedding + memory save).
type ArchiveDeps struct {
	DB            *pgxpool.Pool
	EmbedEndpoint string
	EmbedModel    string
	SubagentFn    memory.SubagentFunc
	GrammarFn     memory.GrammarSubagentFunc
	QueueFn       memory.WorkQueueFunc // background work queue for distillation (preferred over GrammarFn)
}

// SlideAndArchiveContext trims old messages from the StateManager's
// conversation context while archiving them to long-term memory via pgvector.
// It preserves messages[0] (the system prompt) and never splits tool-call
// sequences.
func SlideAndArchiveContext(
	ctx context.Context,
	sm *StateManager,
	maxMessages int,
	deps ArchiveDeps,
) {
	// Step 1 — Early exit if under limit.
	if sm.MessageCount() <= maxMessages {
		return
	}

	// Step 2 — Find safe cutoff (operates on a snapshot).
	// Try backward from the naive cutoff first, then forward. This
	// guarantees the cut never lands inside a tool-call / tool-result pair.
	msgs := sm.ReadMessages()
	naiveCutoff := len(msgs) - maxMessages
	safeIndex := -1

	// Walk backward first (preserves more recent context).
	for i := naiveCutoff; i > 1; i-- {
		if isSafeBoundary(msgs[i]) {
			safeIndex = i
			break
		}
	}

	// If backward search failed, walk forward (removes more but unblocks the slide).
	if safeIndex <= 1 {
		for i := naiveCutoff + 1; i < len(msgs)-1; i++ {
			if isSafeBoundary(msgs[i]) {
				safeIndex = i
				break
			}
		}
	}

	if safeIndex <= 1 {
		logger.Log.Warnf("[slide] no safe cutoff found (naive=%d, msgs=%d); skipping", naiveCutoff, len(msgs))
		return
	}

	// Step 3 — Format archive text from msgs[1:safeIndex].
	// Tool calls are skipped (no informational value). Tool results are
	// condensed to a one-line breadcrumb with the tool name, e.g.:
	//   [search_web → 5 results for "weather Greenville SC"]
	// The assistant's response already synthesizes the full details.
	// A timestamp header gives the distillation model temporal context.
	var b strings.Builder
	fmt.Fprintf(&b, "[Conversation archived %s]\n", timefmt.Now())
	var lastToolName string
	for _, m := range msgs[1:safeIndex] {
		if m.Role == "assistant" && isToolCallContent(m.Content) {
			continue
		}
		// Track tool names from assistant messages for breadcrumb labels.
		if m.Role == "assistant" {
			if name := extractToolName(m.Content); name != "" {
				lastToolName = name
			}
		}
		content := m.Content
		content = stripAgentState(content)
		content = textutil.StripThinkTags(content)
		content = textutil.StripToolIntentTags(content)
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		// Tool results: condense to a one-line breadcrumb with tool name.
		if isToolMessage(m) {
			firstLine, _, _ := strings.Cut(content, "\n")
			if lastToolName != "" {
				content = fmt.Sprintf("[%s → %s]", lastToolName, strings.TrimPrefix(firstLine, "Tool result: "))
				lastToolName = ""
			} else {
				content = firstLine
			}
		}
		if !m.Time.IsZero() {
			fmt.Fprintf(&b, "[%s] %s: %s\n", timefmt.FormatDateTime(m.Time), m.Role, content)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", m.Role, content)
		}
	}

	archiveText := b.String()

	// Step 4 — Async archive (only if there's something to save).
	if archiveText != "" {
		go distillAndSaveArchive(deps, archiveText)
	}

	// Step 5 — Atomic state mutation via StateManager.
	snapshotFP := fingerprintMessages(msgs[1:safeIndex])
	sm.SlideMessages(safeIndex, snapshotFP)
}

// stripAgentState removes the "[Current Agent State]" block that gets
// appended to user messages for LLM context but has no archival value.
func stripAgentState(s string) string {
	if idx := strings.Index(s, "\n\n[Current Agent State]"); idx >= 0 {
		return s[:idx]
	}
	return s
}

// extractToolName pulls the tool name from the first <TOOL_INTENT>tool_name: ...
// tag in an assistant message. Returns "" if no tag is found.
func extractToolName(content string) string {
	const open = "<TOOL_INTENT>"
	idx := strings.Index(content, open)
	if idx < 0 {
		return ""
	}
	after := content[idx+len(open):]
	colonIdx := strings.Index(after, ":")
	closeIdx := strings.Index(after, "</")
	if colonIdx < 0 || (closeIdx >= 0 && closeIdx < colonIdx) {
		return ""
	}
	return strings.TrimSpace(after[:colonIdx])
}

// isSafeBoundary returns true if the message is a safe place to cut:
//   - a user message that is NOT a tool result/error, OR
//   - an assistant message that is NOT a tool call.
func isSafeBoundary(m llm.Message) bool {
	switch m.Role {
	case "user":
		return !isToolMessage(m)
	case "assistant":
		return !isToolCallContent(m.Content)
	default:
		return false
	}
}

// isToolCallContent checks whether content looks like a JSON tool-call object.
// It strips markdown code fences before checking for a JSON object with a "name" key.
func isToolCallContent(content string) bool {
	s := strings.TrimSpace(content)

	// Strip code fences (```json ... ``` or ``` ... ```).
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	if len(s) == 0 || s[0] != '{' || s[len(s)-1] != '}' {
		return false
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return false
	}
	_, hasName := obj["name"]
	return hasName
}

// distilledFact matches the JSON output format of distill_conversation.txt.
type distilledFact struct {
	Text     string   `json:"text"`
	Salience float64  `json:"salience"`
	Tags     []string `json:"tags"`
}

// distillationGrammar constrains conversation distillation output to a valid
// JSON object with a facts array. Without this, Flash models produce malformed
// JSON (spaces inside decimal numbers, trailing dots, etc.).
const distillationGrammar = `root ::= "{" ws "\"facts\":" ws facts ws "}"
facts ::= "[]" | "[" ws fact (ws "," ws fact)* ws "]"
fact ::= "{" ws "\"text\":" ws string "," ws "\"salience\":" ws number "," ws "\"tags\":" ws tags ws "}"
tags ::= "[]" | "[" ws string (ws "," ws string)* ws "]"
string ::= "\"" [^"\\]* "\""
number ::= [0-9] ("." [0-9]+)?
ws ::= [ \t\n\r]*`

// distillAndSaveArchive runs the cleaned archive text through a subagent to
// extract lasting facts, then saves each fact individually. Prefers the work
// queue (QueueFn) which gives each item its own fresh context, preventing
// queue wait time from eating into inference time. Falls back to direct
// GrammarFn/SubagentFn calls, then to raw archive save.
func distillAndSaveArchive(deps ArchiveDeps, archiveText string) {
	if deps.QueueFn == nil && deps.GrammarFn == nil && deps.SubagentFn == nil {
		memory.SaveToMemoryAsync(deps.DB, deps.EmbedEndpoint, deps.EmbedModel, "conversation_archive", archiveText)
		return
	}

	prompt := strings.TrimSpace(prompts.DistillConversation)

	// Prefer the work queue — each item gets its own fresh context, so queue
	// wait time doesn't eat into the inference timeout.
	if deps.QueueFn != nil {
		deps.QueueFn(memory.WorkRequest{
			Label:        "distillation",
			SystemPrompt: prompt,
			UserPrompt:   archiveText,
			Grammar:      distillationGrammar,
			MaxTokens:    2048,
			Timeout:      timeouts.Distillation,
			Retries:      2,
			OnComplete: func(raw string, err error) {
				if err != nil {
					logger.Log.Warnf("[slide] conversation distillation failed: %v; discarding archive", err)
					return
				}
				saveDistilledFacts(deps, raw)
			},
		})
		return
	}

	// Fallback: direct call (no queue available).
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Distillation)
	defer cancel()

	var raw string
	var err error
	if deps.GrammarFn != nil {
		raw, err = deps.GrammarFn(ctx, prompt, archiveText, distillationGrammar)
	} else {
		raw, err = deps.SubagentFn(ctx, prompt, archiveText)
	}
	if err != nil {
		logger.Log.Warnf("[slide] conversation distillation failed: %v; discarding archive", err)
		return
	}
	saveDistilledFacts(deps, raw)
}

// nearDuplicateThreshold is the max cosine distance for two memories to be
// considered near-duplicates. 0.15 is strict — only near-identical semantics.
const nearDuplicateThreshold = 0.15

// saveDistilledFacts parses distillation output, deduplicates within the batch
// and against existing memories, then saves each unique fact.
func saveDistilledFacts(deps ArchiveDeps, raw string) {
	cleaned := textutil.CleanLLMJSON(raw)
	var result struct {
		Facts []distilledFact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		// Partial recovery: try to salvage complete facts from truncated JSON.
		result.Facts = recoverPartialFacts(cleaned)
		if len(result.Facts) == 0 {
			logger.Log.Warnf("[slide] conversation distillation parse failed: %v; discarding archive", err)
			return
		}
		logger.Log.Infof("[slide] recovered %d facts from truncated distillation output", len(result.Facts))
	}

	if len(result.Facts) == 0 {
		logger.Log.Debug("[slide] conversation distillation produced 0 facts; discarding archive")
		return
	}

	// Within-batch dedup by normalized text.
	seen := make(map[string]bool)
	saved := 0
	for _, fact := range result.Facts {
		key := strings.ToLower(strings.TrimSpace(fact.Text))
		if key == "" || fact.Salience < 5 || seen[key] {
			continue
		}
		seen[key] = true

		// Cross-batch dedup: skip if a near-duplicate already exists in DB.
		if deps.DB != nil && deps.EmbedEndpoint != "" {
			if isNearDuplicate(deps, fact.Text) {
				logger.Log.Debugf("[slide] skipping near-duplicate fact: %.60s", fact.Text)
				continue
			}
		}

		req := memory.MemoryWriteRequest{
			Summary:       fact.Text,
			Tags:          fact.Tags,
			Salience:      fact.Salience,
			Source:        "conversation",
			EmbedEndpoint: deps.EmbedEndpoint,
			EmbedModel:    deps.EmbedModel,
		}

		// Use contradiction-checked save when subagent is available so
		// distilled facts don't resurrect information the user corrected.
		// Saves run sequentially (not in parallel goroutines) to avoid
		// saturating the subagent's limited slots with concurrent
		// contradiction checks that would fail with "busy".
		if deps.SubagentFn != nil {
			ctx, cancel := context.WithTimeout(context.Background(), memory.TimeoutSaveOp)
			id, err := memory.CheckAndWriteWithContradiction(ctx, deps.DB, req, deps.SubagentFn, deps.GrammarFn, deps.QueueFn)
			cancel()
			if err != nil {
				logger.Log.Errorf("[slide] contradiction-checked save failed: %v", err)
				continue
			}
			logger.Log.Infof("[slide] contradiction-checked+saved id=%d (salience=%.0f): %.60s", id, req.Salience, req.Summary)
		} else {
			memory.SaveToMemoryWithSalienceAsync(deps.DB, req, deps.GrammarFn)
		}
		saved++
	}
	logger.Log.Infof("[slide] distilled %d facts from conversation archive (%d from batch, %d after dedup)",
		saved, len(result.Facts), saved)
}

// isNearDuplicate embeds the text and checks if a semantically similar memory
// already exists. Returns false on any error (fail-open).
func isNearDuplicate(deps ArchiveDeps, text string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	emb, err := memory.GetEmbedding(ctx, deps.EmbedEndpoint, deps.EmbedModel, text)
	if err != nil {
		return false
	}

	var exists bool
	err = deps.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM memories WHERE (embedding <=> $1) < $2 LIMIT 1)`,
		pgvector.NewVector(emb), nearDuplicateThreshold,
	).Scan(&exists)
	if err != nil {
		return false
	}
	return exists
}

// recoverPartialFacts attempts to salvage complete fact objects from truncated
// JSON output. Finds complete {"text":...,"salience":...,"tags":...} objects
// and parses them individually.
func recoverPartialFacts(s string) []distilledFact {
	var facts []distilledFact
	for {
		// Find the start of the next fact object.
		idx := strings.Index(s, `{"text":`)
		if idx < 0 {
			break
		}
		s = s[idx:]

		// Walk to find the matching closing brace.
		depth := 0
		end := -1
		for i, ch := range s {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					end = i + 1
					break
				}
			}
		}
		if end <= 0 {
			break // truncated mid-object, stop
		}

		var fact distilledFact
		if err := json.Unmarshal([]byte(s[:end]), &fact); err == nil && fact.Text != "" {
			facts = append(facts, fact)
		}
		s = s[end:]
	}
	return facts
}
