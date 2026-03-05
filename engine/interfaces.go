package engine

import "context"

// Notifier sends proactive messages to the user.
type Notifier interface {
	Send(text string)
}

// HotReloader synchronizes on-disk state (skills, routines, shell config).
type HotReloader interface {
	SyncSkills()
	SyncRoutines()
}

// CognitiveServices groups LLM-dependent cognitive operations.
// Nil interface = all cognitive operations disabled.
type CognitiveServices interface {
	Consolidate(ctx context.Context) (int, error)
	Synthesize(ctx context.Context, systemPrompt, content string) (string, error)
	LaunchCuriosity(directive string, priority int, objectiveID int64) (int64, error)
	InferObjectives(ctx context.Context) error
}

// ReflectionSink receives reflection insights for conversation context injection.
type ReflectionSink interface {
	InjectReflection(summary string)
}
