package main

import (
	"context"
	"fmt"

	"sokratos/engine"
	"sokratos/llm"
	"sokratos/platform"
)

// --- Interface Adapters ---

// notifierAdapter implements engine.Notifier by broadcasting messages to all
// allowed recipients via the platform sender.
type notifierAdapter struct {
	sender platform.Sender
}

func (n *notifierAdapter) Send(text string) {
	n.sender.Broadcast(context.Background(), text)
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

// Compile-time interface checks.
var _ engine.Notifier          = (*notifierAdapter)(nil)
var _ engine.HotReloader       = (*hotReloader)(nil)
var _ engine.CognitiveServices = (*cognitiveAdapter)(nil)
var _ engine.ReflectionSink    = (*reflectionSinkAdapter)(nil)
