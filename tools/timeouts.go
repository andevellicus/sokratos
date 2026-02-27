package tools

import (
	"time"

	"sokratos/timeouts"
)

const (
	TimeoutSearXNG            = 15 * time.Second
	TimeoutURLFetch           = 30 * time.Second
	TimeoutMemorySave         = 30 * time.Second
	TimeoutDeepThinker        = 120 * time.Second
	TimeoutText2SQL           = 2 * time.Minute
	TimeoutConversationTriage = 120 * time.Second
	TimeoutSubagentCall       = 10 * time.Second
	TimeoutRetrievalTracking  = 5 * time.Second
	TimeoutInitConsolidation  = 3 * time.Minute
	TimeoutUsefulnessEval     = 10 * time.Second
	TimeoutPrefetch           = 2 * time.Second
	TimeoutDistillation       = 120 * time.Second
	TimeoutTaskExtraction     = 15 * time.Second
	TimeoutSkillExec          = 30 * time.Second
	TimeoutSkillHTTP          = 15 * time.Second
	TimeoutSubagent           = 30 * time.Second
	TimeoutForgetTopic        = 15 * time.Second
	TimeoutTransition         = 90 * time.Second
	TimeoutGatekeeper         = 30 * time.Second
)

// Aliases for shared constants from the timeouts package.
var (
	TimeoutDBQuery   = timeouts.DBQuery
	TimeoutEmbedding = timeouts.Embedding
	TimeoutSynthesis = timeouts.Synthesis
)
