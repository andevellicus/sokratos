package tools

import (
	"context"
	"encoding/json"
	"time"
)

// GetServerTime returns the current server time in RFC3339 format.
func GetServerTime(_ context.Context, _ json.RawMessage) (string, error) {
	return time.Now().UTC().Format(time.RFC3339), nil
}
