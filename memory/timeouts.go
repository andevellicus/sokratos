package memory

import "time"

const (
	TimeoutEmbeddingCall      = 10 * time.Second
	TimeoutSaveOp             = 30 * time.Second
	TimeoutQualityScore       = 10 * time.Second
	TimeoutContradictionCheck = 8 * time.Second
)
