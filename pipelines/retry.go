package pipelines

import (
	"context"
	"time"

	"sokratos/logger"
)

// RetryConfig controls the behavior of RetryWithBackoff.
type RetryConfig struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	LogPrefix      string
	// IsRetryable classifies an error. Returns (true, 0) for immediate retry
	// (e.g. JSON parse errors), (true, >0) to use exponential backoff, or
	// (false, _) for fatal errors that should not be retried.
	IsRetryable func(error) (retry bool, backoff time.Duration)
}

// RetryWithBackoff retries fn up to MaxAttempts times. The IsRetryable function
// on cfg determines whether to retry and how long to wait. Context cancellation
// is respected between attempts.
func RetryWithBackoff[T any](ctx context.Context, cfg RetryConfig, fn func(attempt int) (T, error)) (T, error) {
	backoff := cfg.InitialBackoff
	var zero T

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		result, err := fn(attempt)
		if err == nil {
			return result, nil
		}

		retry, overrideBackoff := cfg.IsRetryable(err)
		if !retry || attempt == cfg.MaxAttempts {
			logger.Log.Errorf("[%s] %v (attempt %d/%d, non-retryable)", cfg.LogPrefix, err, attempt, cfg.MaxAttempts)
			return zero, err
		}

		// Immediate retry (e.g. JSON parse error) — no backoff.
		if overrideBackoff == 0 {
			logger.Log.Warnf("[%s] attempt %d/%d failed, retrying immediately: %v", cfg.LogPrefix, attempt, cfg.MaxAttempts, err)
			continue
		}

		logger.Log.Warnf("[%s] attempt %d/%d failed, retrying in %s: %v", cfg.LogPrefix, attempt, cfg.MaxAttempts, backoff, err)
		select {
		case <-ctx.Done():
			logger.Log.Errorf("[%s] context cancelled while waiting to retry", cfg.LogPrefix)
			return zero, ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
		}
	}

	return zero, nil // unreachable
}
