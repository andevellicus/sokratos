package main

import (
	"context"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"sokratos/logger"
)

const (
	streamEditInterval = 1 * time.Second // min time between Telegram edits
	streamMaxChars     = 4096            // Telegram message char limit
)

// streamSender progressively edits a Telegram message as LLM tokens arrive.
// Intermediate edits use plain text (no parse mode) to avoid unclosed HTML tags;
// only Finalize applies entity-based formatting with expandable_blockquote for thinking.
type streamSender struct {
	bot          *tgbotapi.BotAPI
	chatID       int64
	replyTo      int
	msgID        int // 0 until first chunk sent
	buf          strings.Builder
	lastEdit     time.Time
	typingCancel context.CancelFunc
}

// newStreamSender creates a stream sender for progressive Telegram edits.
// typingCancel is called when the first chunk arrives (replaces typing indicator).
func newStreamSender(bot *tgbotapi.BotAPI, chatID int64, replyTo int, typingCancel context.CancelFunc) *streamSender {
	return &streamSender{
		bot:          bot,
		chatID:       chatID,
		replyTo:      replyTo,
		typingCancel: typingCancel,
	}
}

// OnChunk appends a token to the buffer and sends/edits the Telegram message.
// First call sends a new message; subsequent calls edit it (throttled).
func (s *streamSender) OnChunk(token string) {
	s.buf.WriteString(token)

	if s.msgID == 0 {
		// First chunk: cancel typing indicator and send initial message.
		if s.typingCancel != nil {
			s.typingCancel()
		}
		text := s.truncateForTelegram(s.buf.String() + " ▍")
		msg := tgbotapi.NewMessage(s.chatID, text)
		msg.ReplyToMessageID = s.replyTo
		sent, err := s.bot.Send(msg)
		if err != nil {
			logger.Log.Warnf("[stream] failed to send initial message: %v", err)
			return
		}
		s.msgID = sent.MessageID
		s.lastEdit = time.Now()
		return
	}

	// Throttle edits.
	if time.Since(s.lastEdit) < streamEditInterval {
		return
	}

	text := s.truncateForTelegram(s.buf.String() + " ▍")
	edit := tgbotapi.NewEditMessageText(s.chatID, s.msgID, text)
	if _, err := s.bot.Send(edit); err != nil {
		logger.Log.Debugf("[stream] edit failed (may be unchanged): %v", err)
	}
	s.lastEdit = time.Now()
}

// Finalize replaces the streamed message with the final formatted reply.
// rawAssistant should be the raw LLM output (with think tags) so thinking
// can be shown as a collapsed expandable blockquote. Returns true if a message
// was edited; false if no streaming occurred (caller should send normally).
func (s *streamSender) Finalize(rawAssistant string) bool {
	if s.msgID == 0 {
		return false
	}

	// Final edit with entity-based formatting (expandable_blockquote for thinking).
	fm := formatWithThinking(rawAssistant)
	if err := editFormatted(s.bot, s.chatID, s.msgID, fm); err != nil {
		logger.Log.Warnf("[stream] entity edit failed, falling back to plain text: %v", err)
		_, plain := extractThinking(rawAssistant)
		edit := tgbotapi.NewEditMessageText(s.chatID, s.msgID, plain)
		if _, err := s.bot.Send(edit); err != nil {
			logger.Log.Errorf("[stream] plain text final edit also failed: %v", err)
		}
	}
	return true
}

// HasSent returns true if at least one message was sent to Telegram.
func (s *streamSender) HasSent() bool {
	return s.msgID != 0
}

// Delete removes the streamed message from the chat (e.g. for NO_ACTION_REQUIRED).
func (s *streamSender) Delete() {
	if s.msgID == 0 {
		return
	}
	del := tgbotapi.NewDeleteMessage(s.chatID, s.msgID)
	if _, err := s.bot.Request(del); err != nil {
		logger.Log.Warnf("[stream] failed to delete message: %v", err)
	}
	s.msgID = 0
}

// truncateForTelegram ensures text doesn't exceed Telegram's message limit.
func (s *streamSender) truncateForTelegram(text string) string {
	if len(text) <= streamMaxChars {
		return text
	}
	return text[:streamMaxChars-4] + "\n..."
}
