package main

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	codeBlockRe  = regexp.MustCompile("(?s)```\\w*\n?(.*?)```")
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	boldRe       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italicRe     = regexp.MustCompile(`(?:^|[^*])\*([^*]+?)\*(?:[^*]|$)`)
	strikeRe     = regexp.MustCompile(`~~(.+?)~~`)
	linkRe       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

// mdToTelegramHTML converts common Markdown formatting to Telegram-compatible HTML.
// Code blocks and inline code are extracted first so their contents are protected
// from further transformation.
func mdToTelegramHTML(md string) string {
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

	// 3. Escape HTML entities in remaining text.
	s = escapeHTML(s)

	// 4. Bold: **text** → <b>text</b> (before italic).
	s = boldRe.ReplaceAllString(s, "<b>$1</b>")

	// 5. Italic: *text* → <i>text</i>.
	s = italicRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := italicRe.FindStringSubmatch(match)
		replacement := "<i>" + sub[1] + "</i>"
		// Preserve the boundary characters captured by the lookaround groups.
		prefix := match[:strings.Index(match, "*")]
		suffix := match[strings.LastIndex(match, "*")+1:]
		return prefix + replacement + suffix
	})

	// 6. Strikethrough: ~~text~~ → <s>text</s>.
	s = strikeRe.ReplaceAllString(s, "<s>$1</s>")

	// 7. Links: [text](url) → <a href="url">text</a>.
	s = linkRe.ReplaceAllString(s, `<a href="$2">$1</a>`)

	// 8. Restore placeholders.
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
