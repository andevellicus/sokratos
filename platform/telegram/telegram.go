package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"sokratos/logger"
	"sokratos/platform"
)

// TelegramPlatform implements platform.Platform for the Telegram Bot API.
type TelegramPlatform struct {
	bot          *tgbotapi.BotAPI
	allowedIDs   map[int64]struct{}
	msgChan      chan *platform.IncomingMessage
	callbackChan chan *tgbotapi.CallbackQuery // internal, for Confirm
}

// New creates a TelegramPlatform, starts the update splitter goroutine, and
// begins listening for messages and callback queries.
func New(token string, allowedIDs map[int64]struct{}) (*TelegramPlatform, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	logger.Log.Infof("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	tp := &TelegramPlatform{
		bot:          bot,
		allowedIDs:   allowedIDs,
		msgChan:      make(chan *platform.IncomingMessage, 50),
		callbackChan: make(chan *tgbotapi.CallbackQuery, 10),
	}

	// Splitter goroutine: converts Telegram updates into platform messages
	// and routes callback queries internally.
	go func() {
		for update := range updates {
			if update.CallbackQuery != nil {
				tp.callbackChan <- update.CallbackQuery
			} else if update.Message != nil {
				tp.msgChan <- tp.convertMessage(update.Message)
			}
		}
		close(tp.msgChan)
		close(tp.callbackChan)
	}()

	return tp, nil
}

// convertMessage transforms a tgbotapi.Message into an IncomingMessage,
// downloading photo data if present.
func (tp *TelegramPlatform) convertMessage(msg *tgbotapi.Message) *platform.IncomingMessage {
	im := &platform.IncomingMessage{
		ID:        strconv.Itoa(msg.MessageID),
		ChannelID: strconv.FormatInt(msg.Chat.ID, 10),
		Text:      msg.Text,
		SenderTag: senderTag(msg.From),
		SenderID:  strconv.FormatInt(msg.From.ID, 10),
	}

	// Handle photo messages — download the largest photo.
	if photos := msg.Photo; len(photos) > 0 {
		photo := photos[len(photos)-1]
		data, mime, err := downloadTelegramPhoto(tp.bot, photo.FileID)
		if err != nil {
			logger.Log.Errorf("Failed to download photo: %v", err)
		} else {
			im.PhotoData = data
			im.PhotoMIME = mime
		}
		// Use caption as text for photo messages.
		if msg.Caption != "" {
			im.Text = msg.Caption
		}
	}

	return im
}

// --- Sender ---

// Send sends markdown-formatted text, optionally as reply.
func (tp *TelegramPlatform) Send(ctx context.Context, channelID, markdown, replyTo string) (string, error) {
	chatID, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid channelID %q: %w", channelID, err)
	}

	fm := formatReply(markdown)
	replyToID := 0
	if replyTo != "" {
		replyToID, _ = strconv.Atoi(replyTo)
	}

	sentMsg, sendErr := sendFormatted(tp.bot, chatID, replyToID, fm)
	if sendErr != nil {
		// Fallback to plain text.
		logger.Log.Warnf("Entity send failed, falling back to plain text: %v", sendErr)
		msg := tgbotapi.NewMessage(chatID, markdown)
		if replyToID != 0 {
			msg.ReplyToMessageID = replyToID
		}
		sent, plainErr := tp.bot.Send(msg)
		if plainErr != nil {
			return "", plainErr
		}
		return strconv.Itoa(sent.MessageID), nil
	}
	return strconv.Itoa(sentMsg.MessageID), nil
}

// Edit edits an existing message.
func (tp *TelegramPlatform) Edit(ctx context.Context, channelID, messageID, markdown string) error {
	chatID, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid channelID %q: %w", channelID, err)
	}
	msgID, err := strconv.Atoi(messageID)
	if err != nil {
		return fmt.Errorf("invalid messageID %q: %w", messageID, err)
	}
	fm := formatReply(markdown)
	return editFormatted(tp.bot, chatID, msgID, fm)
}

// StartTyping begins a typing indicator. Returns cancel func.
func (tp *TelegramPlatform) StartTyping(ctx context.Context, channelID string) context.CancelFunc {
	chatID, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		return func() {}
	}
	typingCtx, cancel := context.WithCancel(ctx)
	go sendTypingPeriodically(tp.bot, chatID, typingCtx)
	return cancel
}

// Broadcast sends to all configured recipients.
func (tp *TelegramPlatform) Broadcast(ctx context.Context, markdown string) {
	for id := range tp.allowedIDs {
		chID := strconv.FormatInt(id, 10)
		if _, err := tp.Send(ctx, chID, markdown, ""); err != nil {
			logger.Log.Warnf("Failed to broadcast to %d: %v", id, err)
		}
	}
}

// --- Confirmer ---

// Confirm shows a Telegram inline keyboard with Approve/Deny buttons,
// blocks until a matching callback is received or timeout.
func (tp *TelegramPlatform) Confirm(ctx context.Context, channelID, description string, timeout time.Duration) (bool, error) {
	nonce := fmt.Sprintf("confirm_%d", time.Now().UnixNano())

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Approve", nonce+"_yes"),
			tgbotapi.NewInlineKeyboardButtonData("❌ Deny", nonce+"_no"),
		),
	)

	// Send inline keyboard to all allowed users.
	var sentMsgIDs []int
	for id := range tp.allowedIDs {
		msg := tgbotapi.NewMessage(id, description)
		msg.ReplyMarkup = keyboard
		sent, err := tp.bot.Send(msg)
		if err == nil {
			sentMsgIDs = append(sentMsgIDs, sent.MessageID)
		}
	}

	// removeKeyboard removes the inline keyboard from sent messages.
	removeKeyboard := func(suffix string) {
		for id := range tp.allowedIDs {
			for _, msgID := range sentMsgIDs {
				edit := tgbotapi.NewEditMessageText(id, msgID, description+"\n\n"+suffix)
				tp.bot.Send(edit)
			}
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case cb := <-tp.callbackChan:
			if cb == nil || cb.Data == "" {
				continue
			}
			if len(tp.allowedIDs) > 0 {
				if _, ok := tp.allowedIDs[cb.From.ID]; !ok {
					continue
				}
			}
			if cb.Data != nonce+"_yes" && cb.Data != nonce+"_no" {
				continue
			}
			// Acknowledge the callback.
			ack := tgbotapi.NewCallback(cb.ID, "")
			tp.bot.Send(ack)

			if cb.Data == nonce+"_yes" {
				removeKeyboard("✅ Approved")
				return true, nil
			}
			removeKeyboard("❌ Denied")
			return false, nil
		case <-timer.C:
			removeKeyboard("⏰ Timed out")
			return false, nil
		case <-ctx.Done():
			removeKeyboard("⏰ Cancelled")
			return false, ctx.Err()
		}
	}
}

// --- Receiver ---

// Messages returns the channel of incoming messages.
func (tp *TelegramPlatform) Messages() <-chan *platform.IncomingMessage {
	return tp.msgChan
}

// ReadReply blocks until the next text message arrives.
func (tp *TelegramPlatform) ReadReply() (string, error) {
	for msg := range tp.msgChan {
		if msg.Text != "" {
			return strings.TrimSpace(msg.Text), nil
		}
	}
	return "", fmt.Errorf("message channel closed")
}

// --- CommandRegistrar ---

// RegisterCommands registers Telegram slash commands.
func (tp *TelegramPlatform) RegisterCommands(cmds []platform.Command) error {
	tgCmds := make([]tgbotapi.BotCommand, len(cmds))
	for i, c := range cmds {
		tgCmds[i] = tgbotapi.BotCommand{Command: c.Name, Description: c.Description}
	}
	_, err := tp.bot.Request(tgbotapi.NewSetMyCommands(tgCmds...))
	return err
}

// --- Internal helpers ---

func senderTag(from *tgbotapi.User) string {
	if from.UserName != "" {
		return fmt.Sprintf("@%s", from.UserName)
	}
	name := strings.TrimSpace(from.FirstName + " " + from.LastName)
	if name != "" {
		return fmt.Sprintf("%s (id:%d)", name, from.ID)
	}
	return fmt.Sprintf("id:%d", from.ID)
}

func sendTypingPeriodically(bot *tgbotapi.BotAPI, chatID int64, ctx context.Context) {
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	bot.Send(action)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bot.Send(action)
		}
	}
}

func downloadTelegramPhoto(bot *tgbotapi.BotAPI, fileID string) ([]byte, string, error) {
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return nil, "", fmt.Errorf("get file: %w", err)
	}

	resp, err := http.Get(file.Link(bot.Token))
	if err != nil {
		return nil, "", fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read file: %w", err)
	}

	mime := "image/jpeg"
	switch strings.ToLower(path.Ext(file.FilePath)) {
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	case ".bmp":
		mime = "image/bmp"
	}

	return data, mime, nil
}

// sendFormatted sends a formattedMessage via the Bot API using raw params
// (bypassing the library's MessageEntity struct which lacks expandable_blockquote).
func sendFormatted(bot *tgbotapi.BotAPI, chatID int64, replyTo int, fm formattedMessage) (tgbotapi.Message, error) {
	params := tgbotapi.Params{
		"chat_id": fmt.Sprintf("%d", chatID),
		"text":    fm.Text,
	}
	if replyTo != 0 {
		params["reply_to_message_id"] = fmt.Sprintf("%d", replyTo)
	}
	if s := marshalEntities(fm.Entities); s != "" {
		params["entities"] = s
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
	if s := marshalEntities(fm.Entities); s != "" {
		params["entities"] = s
	}
	_, err := bot.MakeRequest("editMessageText", params)
	return err
}
