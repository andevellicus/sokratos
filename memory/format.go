package memory

import "strings"

// ExtractSummary returns just the triage summary portion of a memory text,
// splitting on the "\n\nSource " delimiter that separates the summary from
// the raw source material (conversation exchange, email, etc.). Returns the
// full text unchanged for memories without a source section (old format,
// distilled facts, reflection output, etc.).
func ExtractSummary(text string) string {
	if idx := strings.Index(text, "\n\nSource "); idx >= 0 {
		return text[:idx]
	}
	return text
}
