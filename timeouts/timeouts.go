package timeouts

import "time"

// Shared timeout constants used by both engine/ and tools/ packages.
const (
	DBQuery      = 5 * time.Second
	Embedding    = 2 * time.Second
	Synthesis    = 3 * time.Minute
	Distillation = 120 * time.Second
	MemorySave   = 30 * time.Second

	// RoutineDB is the timeout for routine-related database operations
	// (seeding defaults, hot-reload upserts).
	RoutineDB = 10 * time.Second

	// HTTPSafetyNet is the transport-level timeout for LLM HTTP clients.
	// Per-request timeouts are controlled via context deadlines; this only
	// catches truly hung connections where context cancellation doesn't
	// propagate to the transport layer.
	HTTPSafetyNet = 5 * time.Minute

	// SnapshotSave is the timeout for conversation snapshot DB writes.
	SnapshotSave = 2 * time.Second

	// SubagentCall is a general timeout for short subagent calls
	// (gatekeeper, share gate, objective eval, image caption).
	SubagentCall = 15 * time.Second

	// LLMWarmup is the timeout for the initial LLM warmup ping.
	LLMWarmup = 30 * time.Second

	// ObjectiveEval is the timeout for objective progress evaluation
	// and related DB writes.
	ObjectiveEval = 30 * time.Second

	// ParadigmShift is the timeout for paradigm shift fast-path
	// (transition memory + mini-consolidation + profile refresh).
	ParadigmShift = 3 * time.Minute

	// ToolSelectionInit is the timeout for the initial batch embedding of
	// all tool descriptions during startup.
	ToolSelectionInit = 10 * time.Second
)
