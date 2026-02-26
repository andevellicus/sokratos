package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
	"sokratos/memory"
)

// SlideAndArchiveContext trims old messages from the StateManager's
// conversation context while archiving them to long-term memory via pgvector.
// It preserves messages[0] (the system prompt) and never splits tool-call
// sequences.
func SlideAndArchiveContext(
	ctx context.Context,
	sm *StateManager,
	maxMessages int,
	db *pgxpool.Pool,
	embedEndpoint string,
	embedModel string,
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
		content := strings.TrimSpace(stripAgentState(m.Content))
		if content == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, content)
	}

	archiveText := b.String()

	// Step 4 — Async archive (only if there's something to save).
	if archiveText != "" {
		memory.SaveToMemoryAsync(db, embedEndpoint, embedModel, "conversation_archive", archiveText)
	}

	// Step 5 — State mutation under write lock with precondition checks.
	snapshotFP := fingerprintMessages(msgs[1:safeIndex])

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Precondition 1: messages haven't shrunk below safeIndex.
	if len(sm.messages) <= safeIndex {
		logger.Log.Warnf("[slide] messages changed (len=%d, safeIndex=%d); aborting", len(sm.messages), safeIndex)
		return
	}

	// Precondition 2: fingerprint of the region we plan to remove still matches.
	currentFP := fingerprintMessages(sm.messages[1:safeIndex])
	if snapshotFP != currentFP {
		logger.Log.Warnf("[slide] fingerprint mismatch; aborting slide")
		return
	}

	// Slide: keep messages[0] (system prompt) + messages[safeIndex:].
	kept := make([]llm.Message, 0, 1+len(sm.messages)-safeIndex)
	kept = append(kept, sm.messages[0])
	kept = append(kept, sm.messages[safeIndex:]...)
	sm.messages = kept

	logger.Log.Infof("[slide] removed %d messages (kept %d)", safeIndex-1, len(sm.messages))
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

// fingerprintMessages returns a SHA-256 hash of the JSON-serialized messages.
func fingerprintMessages(msgs []llm.Message) [32]byte {
	data, _ := json.Marshal(msgs)
	return sha256.Sum256(data)
}

