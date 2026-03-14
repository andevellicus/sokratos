package tools

import (
	"time"

	"sokratos/timeouts"
)

const (
	// --- Network & HTTP ---
	TimeoutSearXNG  = 15 * time.Second
	TimeoutURLFetch = 30 * time.Second

	// --- LLM Backend ---
	TimeoutSubagentCall = 10 * time.Second
	TimeoutDistillation = timeouts.Distillation

	// --- Memory & Triage ---
	TimeoutMemorySave        = timeouts.MemorySave
	TimeoutRetrievalTracking = 5 * time.Second
	TimeoutUsefulnessEval    = 10 * time.Second
	TimeoutPrefetch          = 2 * time.Second
	TimeoutForgetTopic       = 15 * time.Second

	// --- Shell ---
	TimeoutShellAudit = 5 * time.Second

	// --- Dispatch ---
	TimeoutDispatchTriage       = 10 * time.Second
	TimeoutDispatchToolExec     = 5 * time.Minute
	TimeoutDispatchSynthesis    = 30 * time.Second
	TimeoutDispatchDTCSynthesis = 45 * time.Second
	TimeoutMultiStepDispatch    = 90 * time.Second
)
