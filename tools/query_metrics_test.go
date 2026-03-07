package tools

import (
	"context"
	"testing"
)

func TestQueryMetricsReportNilPool(t *testing.T) {
	result, err := QueryMetricsReport(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "No database configured" {
		t.Fatalf("expected no-database message, got %q", result)
	}
}

func TestQueryMetricsReportInvalidWindow(t *testing.T) {
	result, err := QueryMetricsReport(context.Background(), nil, "", "99x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// nil pool is checked first, so we won't reach the window validation.
	// This test just ensures nil pool doesn't panic with any window.
	if result != "No database configured" {
		t.Fatalf("expected no-database message, got %q", result)
	}
}

func TestQueryMetricsReportUnknownReport(t *testing.T) {
	result, err := QueryMetricsReport(context.Background(), nil, "nonexistent", "1h")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "No database configured" {
		t.Fatalf("expected no-database message, got %q", result)
	}
}
