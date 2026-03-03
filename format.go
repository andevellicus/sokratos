package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var (
	codeBlockRe  = regexp.MustCompile("(?s)```\\w*\n?(.*?)```")
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	headingRe    = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	boldRe       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italicRe     = regexp.MustCompile(`(?:^|[^*])\*([^*]+?)\*(?:[^*]|$)`)
	strikeRe     = regexp.MustCompile(`~~(.+?)~~`)
	linkRe       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

// convertTablesToCodeBlocks detects Markdown tables (consecutive lines starting
// with |) and wraps them in ``` fences so the code block handler renders them
// as monospace <pre> blocks in Telegram. Separator rows (|---|...) are stripped.
func convertTablesToCodeBlocks(md string) string {
	lines := strings.Split(md, "\n")
	var result []string
	inTable := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		isTableLine := len(trimmed) > 0 && trimmed[0] == '|' && trimmed[len(trimmed)-1] == '|'
		if isTableLine {
			// Skip separator rows (|---|---|)
			stripped := strings.ReplaceAll(trimmed, "-", "")
			stripped = strings.ReplaceAll(stripped, "|", "")
			stripped = strings.ReplaceAll(stripped, ":", "")
			stripped = strings.TrimSpace(stripped)
			if stripped == "" {
				continue
			}
			if !inTable {
				result = append(result, "```")
				inTable = true
			}
			result = append(result, trimmed)
		} else {
			if inTable {
				result = append(result, "```")
				inTable = false
			}
			result = append(result, line)
		}
	}
	if inTable {
		result = append(result, "```")
	}
	return strings.Join(result, "\n")
}

// mdToTelegramHTML converts common Markdown formatting to Telegram-compatible HTML.
// Code blocks and inline code are extracted first so their contents are protected
// from further transformation.
func mdToTelegramHTML(md string) string {
	md = convertTablesToCodeBlocks(md)
	// 1. Extract code blocks → placeholders.
	var codeBlocks []string
	s := codeBlockRe.ReplaceAllStringFunc(md, func(match string) string {
		inner := codeBlockRe.FindStringSubmatch(match)[1]
		inner = escapeHTML(inner)
		idx := len(codeBlocks)
		codeBlocks = append(codeBlocks, "<pre>"+inner+"</pre>")
		return fmt.Sprintf("\x00CB%d\x00", idx)
	})

	// 2. Extract inline code → placeholders.
	var inlineCodes []string
	s = inlineCodeRe.ReplaceAllStringFunc(s, func(match string) string {
		inner := inlineCodeRe.FindStringSubmatch(match)[1]
		inner = escapeHTML(inner)
		idx := len(inlineCodes)
		inlineCodes = append(inlineCodes, "<code>"+inner+"</code>")
		return fmt.Sprintf("\x00IC%d\x00", idx)
	})

	// 3. Headings: ### text → **text** (converted to bold before HTML escaping).
	s = headingRe.ReplaceAllString(s, "**$1**")

	// 4. Escape HTML entities in remaining text.
	s = escapeHTML(s)

	// 5. Bold: **text** → <b>text</b> (before italic).
	s = boldRe.ReplaceAllString(s, "<b>$1</b>")

	// 6. Italic: *text* → <i>text</i>.
	s = italicRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := italicRe.FindStringSubmatch(match)
		replacement := "<i>" + sub[1] + "</i>"
		// Preserve the boundary characters captured by the lookaround groups.
		prefix := match[:strings.Index(match, "*")]
		suffix := match[strings.LastIndex(match, "*")+1:]
		return prefix + replacement + suffix
	})

	// 7. Strikethrough: ~~text~~ → <s>text</s>.
	s = strikeRe.ReplaceAllString(s, "<s>$1</s>")

	// 8. Links: [text](url) → <a href="url">text</a>.
	s = linkRe.ReplaceAllString(s, `<a href="$2">$1</a>`)

	// 9. Restore placeholders.
	for i, block := range codeBlocks {
		s = strings.Replace(s, fmt.Sprintf("\x00CB%d\x00", i), block, 1)
	}
	for i, code := range inlineCodes {
		s = strings.Replace(s, fmt.Sprintf("\x00IC%d\x00", i), code, 1)
	}

	return s
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}


// telegramEntity is a Bot API MessageEntity with expandable_blockquote support.
// The library's tgbotapi.MessageEntity lacks this type, so we use our own for
// JSON serialization via the entities parameter.
type telegramEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	URL    string `json:"url,omitempty"`
}

// utf16Len returns the length of s in UTF-16 code units (what Telegram uses for offsets).
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// mdToEntities converts Markdown text to plain text + Telegram entity array.
// Handles bold, italic, strikethrough, inline code, code blocks, links, and tables.
func mdToEntities(md string) (string, []telegramEntity) {
	md = convertTablesToCodeBlocks(md)
	var entities []telegramEntity

	// 1. Extract code blocks → placeholders, recording entities.
	type placeholder struct {
		plain    string
		entities []telegramEntity // entities relative to this block's start
	}
	var codeBlocks []placeholder
	s := codeBlockRe.ReplaceAllStringFunc(md, func(match string) string {
		inner := codeBlockRe.FindStringSubmatch(match)[1]
		idx := len(codeBlocks)
		codeBlocks = append(codeBlocks, placeholder{
			plain:    inner,
			entities: []telegramEntity{{Type: "pre", Offset: 0, Length: utf16Len(inner)}},
		})
		return fmt.Sprintf("\x00CB%d\x00", idx)
	})

	// 2. Extract inline code → placeholders.
	var inlineCodes []placeholder
	s = inlineCodeRe.ReplaceAllStringFunc(s, func(match string) string {
		inner := inlineCodeRe.FindStringSubmatch(match)[1]
		idx := len(inlineCodes)
		inlineCodes = append(inlineCodes, placeholder{
			plain:    inner,
			entities: []telegramEntity{{Type: "code", Offset: 0, Length: utf16Len(inner)}},
		})
		return fmt.Sprintf("\x00IC%d\x00", idx)
	})

	// 3. Headings: ### text → **text** (rendered as bold).
	s = headingRe.ReplaceAllString(s, "**$1**")

	// 4. Process inline formatting in a single left-to-right pass.
	// Each match is found at its position in the current string, so
	// entity offsets are always correct relative to the final output.
	s = processAllInlineFormatting(s, &entities)

	// 5. Restore code block placeholders, adjusting offsets.
	for i, cb := range codeBlocks {
		ph := fmt.Sprintf("\x00CB%d\x00", i)
		idx := strings.Index(s, ph)
		if idx < 0 {
			continue
		}
		baseOffset := utf16Len(s[:idx])
		for _, e := range cb.entities {
			e.Offset += baseOffset
			entities = append(entities, e)
		}
		s = strings.Replace(s, ph, cb.plain, 1)
	}

	// 6. Restore inline code placeholders, adjusting offsets.
	for i, ic := range inlineCodes {
		ph := fmt.Sprintf("\x00IC%d\x00", i)
		idx := strings.Index(s, ph)
		if idx < 0 {
			continue
		}
		baseOffset := utf16Len(s[:idx])
		for _, e := range ic.entities {
			e.Offset += baseOffset
			entities = append(entities, e)
		}
		s = strings.Replace(s, ph, ic.plain, 1)
	}

	return s, entities
}

// inlineMatch describes a single formatting match to process.
type inlineMatch struct {
	replaceStart int
	replaceEnd   int
	inner        string
	entityType   string
	url          string
}

// processAllInlineFormatting finds the leftmost formatting match (bold, italic,
// strikethrough, or link), strips its delimiters, records the entity, and repeats.
// Processing one match at a time ensures entity offsets are always relative to the
// current string state, avoiding the offset drift that multi-pass processing causes
// when emojis (UTF-16 surrogate pairs) are present.
func processAllInlineFormatting(s string, entities *[]telegramEntity) string {
	for {
		var best *inlineMatch

		// Bold: **text**
		if loc := boldRe.FindStringIndex(s); loc != nil {
			if sub := boldRe.FindStringSubmatch(s); len(sub) >= 2 {
				m := &inlineMatch{replaceStart: loc[0], replaceEnd: loc[1], inner: sub[1], entityType: "bold"}
				if best == nil || m.replaceStart < best.replaceStart {
					best = m
				}
			}
		}

		// Strikethrough: ~~text~~
		if loc := strikeRe.FindStringIndex(s); loc != nil {
			if sub := strikeRe.FindStringSubmatch(s); len(sub) >= 2 {
				m := &inlineMatch{replaceStart: loc[0], replaceEnd: loc[1], inner: sub[1], entityType: "strikethrough"}
				if best == nil || m.replaceStart < best.replaceStart {
					best = m
				}
			}
		}

		// Links: [text](url)
		if loc := linkRe.FindStringIndex(s); loc != nil {
			if sub := linkRe.FindStringSubmatch(s); len(sub) >= 3 {
				m := &inlineMatch{replaceStart: loc[0], replaceEnd: loc[1], inner: sub[1], entityType: "text_link", url: sub[2]}
				if best == nil || m.replaceStart < best.replaceStart {
					best = m
				}
			}
		}

		// Italic: *text* (boundary-aware to avoid matching **)
		if loc := italicRe.FindStringIndex(s); loc != nil {
			if sub := italicRe.FindStringSubmatch(s); len(sub) >= 2 {
				matchStr := s[loc[0]:loc[1]]
				firstStar := strings.Index(matchStr, "*")
				lastStar := strings.LastIndex(matchStr, "*")
				m := &inlineMatch{
					replaceStart: loc[0] + firstStar,
					replaceEnd:   loc[0] + lastStar + 1,
					inner:        sub[1],
					entityType:   "italic",
				}
				if best == nil || m.replaceStart < best.replaceStart {
					best = m
				}
			}
		}

		if best == nil {
			break
		}

		offset := utf16Len(s[:best.replaceStart])
		*entities = append(*entities, telegramEntity{
			Type:   best.entityType,
			Offset: offset,
			Length: utf16Len(best.inner),
			URL:    best.url,
		})
		s = s[:best.replaceStart] + best.inner + s[best.replaceEnd:]
	}
	return s
}

// formattedMessage holds plain text + entities for sending via the Bot API
// entities parameter (no parse_mode).
type formattedMessage struct {
	Text     string
	Entities []telegramEntity
}

// formatReply builds a formattedMessage by converting Markdown to
// plain text + Telegram entities. Thinking is logged server-side only.
func formatReply(reply string) formattedMessage {
	text, entities := mdToEntities(reply)
	return formattedMessage{Text: text, Entities: entities}
}

// sendFormatted sends a formattedMessage via the Bot API using raw params
// (bypassing the library's MessageEntity struct which lacks expandable_blockquote).
// Falls back to plain text on error.
func sendFormatted(bot *tgbotapi.BotAPI, chatID int64, replyTo int, fm formattedMessage) (tgbotapi.Message, error) {
	params := tgbotapi.Params{
		"chat_id": fmt.Sprintf("%d", chatID),
		"text":    fm.Text,
	}
	if replyTo != 0 {
		params["reply_to_message_id"] = fmt.Sprintf("%d", replyTo)
	}
	if len(fm.Entities) > 0 {
		b, _ := json.Marshal(fm.Entities)
		params["entities"] = string(b)
	}
	resp, err := bot.MakeRequest("sendMessage", params)
	if err != nil {
		return tgbotapi.Message{}, err
	}
	var msg tgbotapi.Message
	if err := json.Unmarshal(resp.Result, &msg); err != nil {
		return tgbotapi.Message{}, err
	}
	return msg, nil
}

// editFormatted edits an existing message with a formattedMessage using raw params.
func editFormatted(bot *tgbotapi.BotAPI, chatID int64, msgID int, fm formattedMessage) error {
	params := tgbotapi.Params{
		"chat_id":    fmt.Sprintf("%d", chatID),
		"message_id": fmt.Sprintf("%d", msgID),
		"text":       fm.Text,
	}
	if len(fm.Entities) > 0 {
		b, _ := json.Marshal(fm.Entities)
		params["entities"] = string(b)
	}
	_, err := bot.MakeRequest("editMessageText", params)
	return err
}

