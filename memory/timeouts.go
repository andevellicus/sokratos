package memory

import (
	"time"

	"sokratos/timeouts"
)

const (
	TimeoutEmbeddingCall       = 10 * time.Second
	TimeoutSaveOp              = timeouts.MemorySave
	TimeoutQualityScore        = 20 * time.Second
	TimeoutContradictionCheck  = 20 * time.Second
	TimeoutQualityEnrich       = 30 * time.Second
)
