package pipelines

import (
	"time"

	"sokratos/timeouts"
)

const (
	// --- Memory & Triage ---
	TimeoutConversationTriage = 45 * time.Second
	TimeoutMemorySave         = timeouts.MemorySave

	// --- Bootstrap ---
	TimeoutBootstrapProfile = 5 * time.Minute

	// --- Consolidation ---
	TimeoutConsolidationDefault = 10 * time.Minute
	TimeoutInitConsolidation    = 3 * time.Minute
	TimeoutMiniConsolidation    = 90 * time.Second
)
