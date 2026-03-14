package orchestrate

import (
	"sokratos/textutil"
)

// processThinking extracts thinking content from a raw LLM response, returning
// the clean content (think-stripped) and the stored version (with visible reasoning prefix).
// Qwen3.5's Jinja template strips <think> blocks from historical messages, so we
// convert thinking to a visible [Reasoning: ...] prefix that persists across rounds —
// letting subsequent rounds see prior reasoning without paying extra inference cost.
func processThinking(raw string) (content, stored, thinking string) {
	thinking = textutil.ExtractThinkContent(raw)
	content = textutil.StripThinkTags(raw)
	stored = content
	if thinking != "" {
		stored = "[Reasoning: " + thinking + "]\n" + content
	}
	return
}

// prepareThinking configures the grammar and reasoning format for a round
// based on whether thinking is enabled.
func prepareThinking(grammar string, thinkEnabled bool) (roundGrammar, reasoningFmt string) {
	roundGrammar = grammar
	reasoningFmt = "none"
	if thinkEnabled {
		if grammar != "" {
			roundGrammar = textutil.WrapGrammarWithThinkBlock(grammar)
		} else {
			reasoningFmt = "deepseek" // no grammar → server-side split
		}
	}
	return
}

