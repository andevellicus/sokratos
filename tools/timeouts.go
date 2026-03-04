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
	TimeoutSubagentCall = 10 * time.Second
	TimeoutDistillation = timeouts.Distillation

	// --- Memory & Triage ---
	TimeoutMemorySave        = timeouts.MemorySave
	TimeoutRetrievalTracking = 5 * time.Second
	TimeoutUsefulnessEval    = 10 * time.Second
	TimeoutPrefetch          = 2 * time.Second
	TimeoutForgetTopic       = 15 * time.Second

	// --- Tasks & Skills ---
	TimeoutSkillExec           = 30 * time.Second
	TimeoutSkillExecDelegation = 5 * time.Minute
	TimeoutSkillKV             = 5 * time.Second
	TimeoutDelegateCall        = 60 * time.Second
	TimeoutDelegateBatch       = 3 * time.Minute

	// --- Shell ---
	TimeoutShellAudit = 5 * time.Second

	// --- Plan Execution ---
	TimeoutPlanDecomposition = 60 * time.Second
	TimeoutPlanStepExecution = 90 * time.Second
	TimeoutPlanForeground    = 5 * time.Minute
	TimeoutPlanBackground    = 15 * time.Minute
	TimeoutPlanProgressDB    = 5 * time.Second
)
