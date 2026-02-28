package httputil

import (
	"net/http"
	"time"
)

// NewClient returns an http.Client with the given timeout and shared
// transport defaults (idle connection pooling, keep-alive). Different
// callers intentionally use different timeout values; this factory
// centralizes the transport configuration.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}
