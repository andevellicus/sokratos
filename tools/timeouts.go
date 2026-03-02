package tools

import (
	"time"

	"sokratos/timeouts"
)

const (
	// --- Network & HTTP ---
	TimeoutSearXNG   = 15 * time.Second
	TimeoutURLFetch  = 30 * time.Second
	TimeoutSkillHTTP = 15 * time.Second

	// --- LLM Backend ---
	TimeoutDeepThinker  = 4 * time.Minute
	TimeoutSubagent     = 30 * time.Second
	TimeoutSubagentCall = 10 * time.Second
	TimeoutGatekeeper   = 30 * time.Second
	TimeoutDistillation = timeouts.Distillation

	// --- Memory & Triage ---
	TimeoutMemorySave         = timeouts.MemorySave
	TimeoutConversationTriage = 45 * time.Second
	TimeoutRetrievalTracking  = 5 * time.Second
	TimeoutUsefulnessEval     = 10 * time.Second
	TimeoutPrefetch           = 2 * time.Second
	TimeoutForgetTopic        = 15 * time.Second
	TimeoutTransition         = 90 * time.Second
	TimeoutInitConsolidation  = 3 * time.Minute

	// --- Tasks & Skills ---
	TimeoutTaskExtraction = 15 * time.Second
	TimeoutSkillExec      = 30 * time.Second
	TimeoutSkillKV        = 5 * time.Second

	// --- Bootstrap ---
	TimeoutBootstrapProfile = 5 * time.Minute

	// --- Consolidation ---
	TimeoutConsolidationDefault = 10 * time.Minute

	// --- Plan Execution ---
	TimeoutPlanDecomposition = 60 * time.Second
	TimeoutPlanStepExecution = 90 * time.Second
	TimeoutPlanForeground    = 5 * time.Minute
	TimeoutPlanBackground    = 15 * time.Minute
	TimeoutPlanProgressDB    = 5 * time.Second

	// --- HTTP Safety Net ---
	TimeoutHTTPSafetyNet = timeouts.HTTPSafetyNet
)

// Aliases for shared constants from the timeouts package.
var (
	TimeoutDBQuery   = timeouts.DBQuery
	TimeoutEmbedding = timeouts.Embedding
	TimeoutSynthesis = timeouts.Synthesis
)
