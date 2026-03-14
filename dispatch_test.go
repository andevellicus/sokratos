package main

import (
	"os"
	"testing"

	"sokratos/engine"
	"sokratos/logger"
)

func TestMain(m *testing.M) {
	// Initialize logger to avoid nil pointer in Registry.Register.
	logger.Init(os.TempDir())
	os.Exit(m.Run())
}

func TestBuildJobContext(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		result := buildJobContext(nil)
		if result != "" {
			t.Errorf("expected empty string, got %q", result)
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		result := buildJobContext([]*engine.BackgroundJob{})
		if result != "" {
			t.Errorf("expected empty string for empty slice, got %q", result)
		}
	})
}
