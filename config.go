package main

import (
	"os"
	"strconv"
	"time"

	"sokratos/logger"
)

// envString returns the value of the environment variable named by key, or def
// if the variable is not set or empty.
func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt returns the integer value of the environment variable named by key,
// or def if the variable is not set, empty, or cannot be parsed.
func envInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logger.Log.Warnf("Invalid %s %q, using default %d", key, raw, def)
		return def
	}
	return n
}

// envFloat returns the float64 value of the environment variable named by key,
// or def if the variable is not set, empty, or cannot be parsed.
func envFloat(key string, def float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		logger.Log.Warnf("Invalid %s %q, using default %.1f", key, raw, def)
		return def
	}
	return v
}

// envDuration returns the time.Duration value of the environment variable
// named by key, or def if the variable is not set, empty, or cannot be parsed.
func envDuration(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		logger.Log.Warnf("Invalid %s %q, using default %s", key, raw, def)
		return def
	}
	return d
}
