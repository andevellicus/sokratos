package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
	"sokratos/prompts"
	"sokratos/textutil"
)

// ArchiveDeps groups dependencies for context archival (embedding + memory save).
type ArchiveDeps struct {
	DB            *pgxpool.Pool
	EmbedEndpoint string
	EmbedModel    string
	SubagentFn    memory.SubagentFunc
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
	// Tool calls (the JSON invocation) are skipped since they carry no
	// informational value, but tool results ARE included — they contain
	// the actual data from search_web, read_url, etc.
	var b strings.Builder
	for _, m := range msgs[1:safeIndex] {
		if m.Role == "assistant" && isToolCallContent(m.Content) {
			continue
		}
		content := m.Content
		content = stripAgentState(content)
		content = textutil.StripThinkTags(content)
		content = textutil.StripToolIntentTags(content)
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, content)
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

// distillAndSaveArchive runs the cleaned archive text through a subagent to
// extract lasting facts, then saves each fact individually. Falls back to
// saving the raw cleaned archive if SubagentFn is nil.
func distillAndSaveArchive(deps ArchiveDeps, archiveText string) {
	if deps.SubagentFn == nil {
		memory.SaveToMemoryAsync(deps.DB, deps.EmbedEndpoint, deps.EmbedModel, "conversation_archive", archiveText)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := deps.SubagentFn(ctx, strings.TrimSpace(prompts.DistillConversation), archiveText)
	if err != nil {
		logger.Log.Warnf("[slide] conversation distillation failed: %v; discarding archive", err)
		return
	}

	cleaned := textutil.CleanLLMJSON(raw)
	var result struct {
		Facts []distilledFact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		logger.Log.Warnf("[slide] conversation distillation parse failed: %v; discarding archive", err)
		return
	}

	if len(result.Facts) == 0 {
		logger.Log.Debug("[slide] conversation distillation produced 0 facts; discarding archive")
		return
	}

	for _, fact := range result.Facts {
		if strings.TrimSpace(fact.Text) == "" || fact.Salience < 5 {
			continue
		}
		memory.SaveToMemoryWithSalienceAsync(deps.DB, memory.MemoryWriteRequest{
			Summary:       fact.Text,
			Tags:          fact.Tags,
			Salience:      fact.Salience,
			Source:        "conversation",
			EmbedEndpoint: deps.EmbedEndpoint,
			EmbedModel:    deps.EmbedModel,
		}, deps.SubagentFn)
	}
	logger.Log.Infof("[slide] distilled %d facts from conversation archive", len(result.Facts))
}
