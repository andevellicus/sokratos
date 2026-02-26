package engine

import "time"

const (
	TimeoutDBQuery  = 5 * time.Second
	TimeoutEmbedding = 2 * time.Second
	TimeoutSynthesis = 3 * time.Minute
)
