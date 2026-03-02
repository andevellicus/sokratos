package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"sokratos/logger"
)

// formatConfirmation builds a human-readable confirmation prompt for a tool call.
func formatConfirmation(name string, args json.RawMessage) string {
	switch name {
	case "send_email":
		var a struct {
			To      string `json:"to"`
			Subject string `json:"subject"`
		}
		_ = json.Unmarshal(args, &a)
		return fmt.Sprintf("⚠️ Send email to %s\nSubject: %q", a.To, a.Subject)
	case "create_event":
		var a struct {
			Title string `json:"title"`
			Start string `json:"start"`
		}
		_ = json.Unmarshal(args, &a)
		return fmt.Sprintf("⚠️ Create calendar event\n%q at %s", a.Title, a.Start)
	case "create_skill":
		var a struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(args, &a)
		return fmt.Sprintf("⚠️ Create skill %q\n%s", a.Name, a.Description)
	default:
		return fmt.Sprintf("⚠️ Execute %s?", name)
	}
}

// approvalCacheKey builds a cache key that identifies the specific action being
// approved, not just the tool name. For create_skill it includes the skill name,
// for send_email the recipient+subject, etc. Tools without specific identity
// fields fall back to just the tool name.
func approvalCacheKey(name string, args json.RawMessage) string {
	switch name {
	case "create_skill":
		var a struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(args, &a) == nil && a.Name != "" {
			return name + ":" + a.Name
		}
	case "send_email":
		var a struct {
			To      string `json:"to"`
			Subject string `json:"subject"`
		}
		if json.Unmarshal(args, &a) == nil {
			return name + ":" + a.To + ":" + a.Subject
		}
	case "create_event":
		var a struct {
			Title string `json:"title"`
			Start string `json:"start"`
		}
		if json.Unmarshal(args, &a) == nil {
			return name + ":" + a.Title + ":" + a.Start
		}
	}
	return name
}

// confirmToolExec wraps a tool executor with Telegram inline-keyboard
// confirmation for externally-visible actions. When a gated tool is called,
// it sends an inline keyboard with Approve/Deny buttons and waits for a
// CallbackQuery on the dedicated callbacks channel (or 2-minute timeout).
// Approvals are cached for 5 minutes keyed on tool name + identifying arguments,
// so retries of the same action auto-approve but different actions re-prompt.
func confirmToolExec(
	base func(context.Context, json.RawMessage) (string, error),
	bot *tgbotapi.BotAPI,
	callbacks <-chan *tgbotapi.CallbackQuery,
	allowedIDs map[int64]struct{},
	confirmTools map[string]bool,
	confirmTimeout time.Duration,
) func(context.Context, json.RawMessage) (string, error) {
	const approvalTTL = 5 * time.Minute

	var approvalMu sync.Mutex
	approvals := make(map[string]time.Time)

	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &call); err == nil && confirmTools[call.Name] {
			cacheKey := approvalCacheKey(call.Name, call.Arguments)

			// Check approval cache.
			approvalMu.Lock()
			if t, ok := approvals[cacheKey]; ok && time.Since(t) < approvalTTL {
				approvalMu.Unlock()
				logger.Log.Debugf("[confirm] auto-approved %s (cached %s ago)", cacheKey, time.Since(t).Round(time.Second))
				return base(ctx, raw)
			}
			approvalMu.Unlock()
			desc := formatConfirmation(call.Name, call.Arguments)
			nonce := fmt.Sprintf("confirm_%d", time.Now().UnixNano())

			keyboard := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("✅ Approve", nonce+"_yes"),
					tgbotapi.NewInlineKeyboardButtonData("❌ Deny", nonce+"_no"),
				),
			)

			// Send inline keyboard to all allowed users.
			var sentMsgIDs []int // track sent message IDs for cleanup
			for id := range allowedIDs {
				msg := tgbotapi.NewMessage(id, desc)
				msg.ReplyMarkup = keyboard
				sent, err := bot.Send(msg)
				if err == nil {
					sentMsgIDs = append(sentMsgIDs, sent.MessageID)
				}
			}

			// removeKeyboard removes the inline keyboard from sent messages after
			// a response is received, leaving only the confirmation text.
			removeKeyboard := func(suffix string) {
				for id := range allowedIDs {
					for _, msgID := range sentMsgIDs {
						edit := tgbotapi.NewEditMessageText(id, msgID, desc+"\n\n"+suffix)
						bot.Send(edit)
					}
				}
			}

			timer := time.NewTimer(confirmTimeout)
			defer timer.Stop()
			for {
				select {
				case cb := <-callbacks:
					if cb == nil || cb.Data == "" {
						continue
					}
					if len(allowedIDs) > 0 {
						if _, ok := allowedIDs[cb.From.ID]; !ok {
							continue
						}
					}
					// Only respond to our nonce.
					if cb.Data != nonce+"_yes" && cb.Data != nonce+"_no" {
						continue
					}
					// Acknowledge the callback to dismiss the spinner.
					ack := tgbotapi.NewCallback(cb.ID, "")
					bot.Send(ack)

					if cb.Data == nonce+"_yes" {
						removeKeyboard("✅ Approved")
						approvalMu.Lock()
						approvals[cacheKey] = time.Now()
						approvalMu.Unlock()
						return base(ctx, raw)
					}
					removeKeyboard("❌ Denied")
					return "Action cancelled by user.", nil
				case <-timer.C:
					removeKeyboard("⏰ Timed out")
					return "Action cancelled — confirmation timed out.", nil
				}
			}
		}
		return base(ctx, raw)
	}
}
