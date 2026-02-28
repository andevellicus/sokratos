package main

import (
	"strings"
	"testing"

	"sokratos/llm"
)

func TestCondenseToolResults_BasicCondensing(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "What's the weather?"},
		{Role: "assistant", Content: "Let me check. <TOOL_INTENT>search_web: {\"query\": \"weather\"}</TOOL_INTENT>"},
		{Role: "user", Content: "Tool result: Current weather in Berlin: 15°C, partly cloudy with a chance of rain in the afternoon. Wind: 10km/h NW."},
		{Role: "assistant", Content: "The weather in Berlin is 15°C and partly cloudy."},
	}

	condensed := condenseToolResults(msgs)

	// The tool result at index 2 should be condensed.
	if !strings.HasPrefix(condensed[2].Content, "Tool result: [search_web -> ") {
		t.Errorf("expected condensed tool result, got: %s", condensed[2].Content)
	}
	// Original messages should be unchanged.
	if condensed[0].Content != msgs[0].Content {
		t.Error("user message should be unchanged")
	}
	if condensed[3].Content != msgs[3].Content {
		t.Error("final assistant message should be unchanged")
	}
}

func TestCondenseToolResults_PreservesLastToolResult(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "Search for news"},
		{Role: "assistant", Content: "<TOOL_INTENT>search_web: {\"query\": \"news\"}</TOOL_INTENT>"},
		{Role: "user", Content: "Tool result: Latest news headlines for today"},
	}

	condensed := condenseToolResults(msgs)

	// No assistant after the tool result, so it should NOT be condensed.
	if condensed[2].Content != msgs[2].Content {
		t.Errorf("last tool result should be preserved, got: %s", condensed[2].Content)
	}
}

func TestCondenseToolResults_PreservesToolErrors(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "Check email"},
		{Role: "assistant", Content: "<TOOL_INTENT>search_email: {}</TOOL_INTENT>"},
		{Role: "user", Content: "Tool error: connection timeout"},
		{Role: "assistant", Content: "Sorry, I couldn't check your email due to a timeout."},
	}

	condensed := condenseToolResults(msgs)

	// Tool errors should never be condensed.
	if condensed[2].Content != msgs[2].Content {
		t.Errorf("tool error should be preserved, got: %s", condensed[2].Content)
	}
}

func TestCondenseToolResults_MultipleRounds(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "Brief me"},
		{Role: "assistant", Content: "<TOOL_INTENT>search_email: {}</TOOL_INTENT>"},
		{Role: "user", Content: "Tool result: You have 3 new emails from Alice, Bob, and Charlie about project updates and deadlines."},
		{Role: "assistant", Content: "<TOOL_INTENT>search_calendar: {}</TOOL_INTENT>"},
		{Role: "user", Content: "Tool result: Meeting at 2pm with the team to discuss quarterly goals and planning."},
		{Role: "assistant", Content: "Here's your briefing: 3 new emails and a meeting at 2pm."},
	}

	condensed := condenseToolResults(msgs)

	// Both intermediate tool results should be condensed.
	if !strings.Contains(condensed[2].Content, "[search_email -> ") {
		t.Errorf("first tool result should be condensed, got: %s", condensed[2].Content)
	}
	if !strings.Contains(condensed[4].Content, "[search_calendar -> ") {
		t.Errorf("second tool result should be condensed, got: %s", condensed[4].Content)
	}
	// Final assistant preserved.
	if condensed[5].Content != msgs[5].Content {
		t.Error("final assistant should be unchanged")
	}
}

func TestCondenseToolResults_NoToolResults(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
	}

	condensed := condenseToolResults(msgs)

	if len(condensed) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(condensed))
	}
	for i := range msgs {
		if condensed[i].Content != msgs[i].Content {
			t.Errorf("message %d changed unexpectedly", i)
		}
	}
}

func TestCondenseToolResults_LongFirstLine(t *testing.T) {
	longResult := "Tool result: " + strings.Repeat("x", 200)
	msgs := []llm.Message{
		{Role: "assistant", Content: "<TOOL_INTENT>search_web: {}</TOOL_INTENT>"},
		{Role: "user", Content: longResult},
		{Role: "assistant", Content: "Done."},
	}

	condensed := condenseToolResults(msgs)

	// The condensed result should have the first line capped at maxCondenseFirstLine.
	body := strings.TrimPrefix(condensed[1].Content, "Tool result: ")
	// [search_web -> xxx...xxx...]
	if !strings.HasSuffix(body, "...]") {
		t.Errorf("expected truncated first line, got: %s", body)
	}
}

func TestCondenseToolResults_EmptySlice(t *testing.T) {
	condensed := condenseToolResults(nil)
	if condensed != nil {
		t.Error("expected nil for nil input")
	}
}
