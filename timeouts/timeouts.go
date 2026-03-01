package timeouts

import "time"

// Shared timeout constants used by both engine/ and tools/ packages.
const (
	DBQuery      = 5 * time.Second
	Embedding    = 2 * time.Second
	Synthesis    = 3 * time.Minute
	Distillation = 120 * time.Second
	MemorySave   = 30 * time.Second

	// PersonalityMigration is the timeout for the one-time personality trait
	// migration from monolithic profile on startup.
	PersonalityMigration = 30 * time.Second

	// RoutineDB is the timeout for routine-related database operations
	// (seeding defaults, hot-reload upserts).
	RoutineDB = 10 * time.Second

	// HTTPSafetyNet is the transport-level timeout for LLM HTTP clients.
	// Per-request timeouts are controlled via context deadlines; this only
	// catches truly hung connections where context cancellation doesn't
	// propagate to the transport layer.
	HTTPSafetyNet = 5 * time.Minute
)
