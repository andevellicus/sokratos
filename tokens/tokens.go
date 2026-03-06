// Package tokens defines named constants for LLM maxTokens values used
// across the codebase. Centralising these makes it easy to tune token
// budgets without hunting through call sites.
package tokens

const (
	// --- Subagent (9B) ---

	// SubagentGeneral is the default for general-purpose subagent calls
	// (query rewriting, triage, re-ranking, contradiction checks).
	SubagentGeneral = 1024

	// WebSummary is for subagent web page content summarization in search_web.
	WebSummary = 1024

	// EmailSummary is for subagent email body summarization in search_email.
	EmailSummary = 512

	// SubagentSupervisor is the per-round budget for SubagentSupervisor
	// tool-dispatch loops.
	SubagentSupervisor = 2048

	// QueryRewrite is for subagent query-rewriting (3 search variations).
	QueryRewrite = 512

	// Rerank is for subagent re-ranking of memory search results.
	Rerank = 256

	// SQLGen is for natural-language-to-SQL generation.
	SQLGen = 512

	// --- Gatekeeper (grammar-constrained decisions) ---

	// GatekeeperDecision is for heartbeat gatekeeper and curiosity gate
	// (grammar-constrained action/skip JSON).
	GatekeeperDecision = 256

	// ObjectiveEval is for grammar-constrained objective progress evaluation.
	ObjectiveEval = 512

	// ShareGate is for grammar-constrained share quality assessment.
	ShareGate = 256

	// --- Deep Thinker (122B Brain) ---

	// DTCDefault is the default maxTokens for consult_deep_thinker when
	// the caller doesn't specify.
	DTCDefault = 2048

	// DTCSynthesis is for DTC synthesis calls (episodes, reflection).
	DTCSynthesis = 2048

	// DTCTransition is for paradigm-shift transition memory generation.
	DTCTransition = 1024

	// Distillation is for archive distillation WorkRequests.
	Distillation = 2048

	// --- Plan Execution ---

	// PlanDecomposition is for DTC plan decomposition and re-planning.
	PlanDecomposition = 2048

	// PlanStep is for complex plan step execution via DTC (thinking enabled).
	PlanStep = 4096

	// --- Pipelines ---

	// TriageDTC is for DTC conversation triage (grammar-constrained scoring).
	TriageDTC = 2048

	// PreferenceExtract is for preference extraction fast-path.
	PreferenceExtract = 512

	// Consolidation is for memory consolidation batch output.
	Consolidation = 4096

	// BootstrapProfile is for initial bootstrap dual-output JSON.
	BootstrapProfile = 8192

	// ObjectiveInference is for DTC objective inference from memory.
	ObjectiveInference = 2048

	// --- Memory WorkRequests ---

	// MemoryEnrichment is for background memory work (contradiction checks,
	// quality scoring, entity extraction).
	MemoryEnrichment = 512
)
