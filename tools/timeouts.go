package tools

import "time"

const (
	TimeoutSearXNG            = 15 * time.Second
	TimeoutURLFetch           = 30 * time.Second
	TimeoutMemorySave         = 30 * time.Second
	TimeoutDeepThinker        = 120 * time.Second
	TimeoutText2SQL           = 2 * time.Minute
	TimeoutConversationTriage = 120 * time.Second
	TimeoutGraniteCall        = 10 * time.Second
	TimeoutRetrievalTracking  = 5 * time.Second
	TimeoutDBQuery            = 5 * time.Second
	TimeoutEmbedding          = 2 * time.Second
	TimeoutSynthesis          = 3 * time.Minute
	TimeoutInitConsolidation  = 3 * time.Minute
	TimeoutUsefulnessEval     = 10 * time.Second
	TimeoutPrefetch           = 2 * time.Second
)
