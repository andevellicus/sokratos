package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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

// sendTypingPeriodically sends a "typing..." chat action immediately and then
// every 5 seconds until ctx is cancelled.
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

// downloadTelegramPhoto fetches a photo from Telegram's servers and returns
// the raw bytes along with the detected MIME type.
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
