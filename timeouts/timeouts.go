package timeouts

import "time"

// Shared timeout constants used by both engine/ and tools/ packages.
const (
	DBQuery   = 5 * time.Second
	Embedding = 2 * time.Second
	Synthesis = 3 * time.Minute
)
