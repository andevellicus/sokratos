package main

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"sokratos/engine"
	"sokratos/llm"
	"sokratos/logger"
)

// --- Interface Adapters ---

// notifierAdapter implements engine.Notifier by sending messages to all allowed
// Telegram user IDs with HTML formatting and fallback to plain text.
type notifierAdapter struct {
	bot        *tgbotapi.BotAPI
	allowedIDs map[int64]struct{}
}

func (n *notifierAdapter) Send(text string) {
	for id := range n.allowedIDs {
		msg := tgbotapi.NewMessage(id, mdToTelegramHTML(text))
		msg.ParseMode = tgbotapi.ModeHTML
		if _, err := n.bot.Send(msg); err != nil {
			msg.Text = text
			msg.ParseMode = ""
			if _, err := n.bot.Send(msg); err != nil {
				logger.Log.Errorf("Failed to send scheduled message to %d: %v", id, err)
			}
		}
	}
}

// hotReloader implements engine.HotReloader.
type hotReloader struct {
	syncSkills   func()
	syncRoutines func()
}

func (r *hotReloader) SyncSkills()   { r.syncSkills() }
func (r *hotReloader) SyncRoutines() { r.syncRoutines() }

// cognitiveAdapter implements engine.CognitiveServices.
type cognitiveAdapter struct {
	consolidate     func(ctx context.Context) (int, error)
	synthesize      func(ctx context.Context, systemPrompt, content string) (string, error)
	launchCuriosity func(directive string, priority int, objectiveID int64) (int64, error)
	inferObjectives func(ctx context.Context) error
}

func (c *cognitiveAdapter) Consolidate(ctx context.Context) (int, error) {
	if c.consolidate == nil {
		return 0, nil
	}
	return c.consolidate(ctx)
}
func (c *cognitiveAdapter) Synthesize(ctx context.Context, systemPrompt, content string) (string, error) {
	if c.synthesize == nil {
		return "", fmt.Errorf("synthesize not configured")
	}
	return c.synthesize(ctx, systemPrompt, content)
}
func (c *cognitiveAdapter) LaunchCuriosity(directive string, priority int, objectiveID int64) (int64, error) {
	if c.launchCuriosity == nil {
		return 0, fmt.Errorf("curiosity not configured")
	}
	return c.launchCuriosity(directive, priority, objectiveID)
}
func (c *cognitiveAdapter) InferObjectives(ctx context.Context) error {
	if c.inferObjectives == nil {
		return nil
	}
	return c.inferObjectives(ctx)
}

// reflectionSinkAdapter implements engine.ReflectionSink.
type reflectionSinkAdapter struct {
	sm *engine.StateManager
}

func (r *reflectionSinkAdapter) InjectReflection(summary string) {
	r.sm.AppendMessage(llm.Message{
		Role:    "user",
		Content: "[REFLECTION] A pattern was identified from recent memories:\n" + summary + "\nUse this if relevant to future interactions.",
	})
}
