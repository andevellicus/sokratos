package main

import (
	"strings"
	"testing"

	"sokratos/llm"
)

func TestExtractToolName(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"clean JSON", `{"action":"tool","name":"search_web","arguments":{}}`, "search_web"},
		{"with reasoning prefix", `[Reasoning: thinking about it]
{"action":"tool","name":"search_email","arguments":{}}`, "search_email"},
		{"respond action", `{"action":"respond","text":"hello"}`, "unknown"},
		{"garbage", "not json at all", "unknown"},
		{"empty", "", "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractToolName(tc.content)
			if got != tc.want {
				t.Errorf("extractToolName(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

func TestCondenseToolResults_ShortResultPreserved(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "What's the weather?"},
		{Role: "assistant", Content: `{"action":"tool","name":"search_web","arguments":{"query":"weather"}}`},
		{Role: "user", Content: "Tool result: Current weather in Berlin: 15°C, partly cloudy."},
		{Role: "assistant", Content: `{"action":"respond","text":"The weather in Berlin is 15°C and partly cloudy."}`},
	}

	condensed := condenseToolResults(msgs)

	// Short results (under maxCondensedLen) are left as-is.
	if condensed[2].Content != msgs[2].Content {
		t.Errorf("short tool result should be preserved, got: %s", condensed[2].Content)
	}
}

func TestCondenseToolResults_LongResultCondensed(t *testing.T) {
	longBody := "1. Ophelia (2018 film) - Wikipedia\n   https://en.wikipedia.org/wiki/Ophelia\n   Based on the 2006 novel...\n" +
		"2. The Fate of Ophelia - Wikipedia\n   https://en.wikipedia.org/wiki/The_Fate_of_Ophelia\n   Song by Taylor Swift from The Life of a Showgirl...\n" +
		"3. Hamlet - Shakespeare\n   https://example.com/hamlet\n   The famous tragedy featuring Ophelia...\n" +
		"4. Paris Paloma - Songs\n   https://example.com/paris\n   Gothic folk artist known for literary themes...\n" +
		"5. Lana Del Rey - Discography\n   https://example.com/lana\n   American singer known for cinematic pop...\n"
	msgs := []llm.Message{
		{Role: "user", Content: "Search for it"},
		{Role: "assistant", Content: `{"action":"tool","name":"search_web","arguments":{"query":"The Fate of Ophelia"}}`},
		{Role: "user", Content: "Tool result: " + longBody},
		{Role: "assistant", Content: `{"action":"respond","text":"I found several results about The Fate of Ophelia."}`},
	}

	condensed := condenseToolResults(msgs)

	result := condensed[2].Content
	if !strings.HasPrefix(result, "Tool result: [search_web] ") {
		t.Errorf("expected condensed prefix, got: %s", result)
	}
	if !strings.HasSuffix(result, "...") {
		t.Errorf("expected truncation suffix, got: %s", result)
	}
	if !strings.Contains(result, "Ophelia (2018 film)") {
		t.Error("condensed result should contain first result title")
	}
	if !strings.Contains(result, "The Fate of Ophelia") {
		t.Error("condensed result should contain second result title")
	}
	if len(result) >= len(msgs[2].Content) {
		t.Error("condensed result should be shorter than original")
	}
}

func TestCondenseToolResults_PreservesLastToolResult(t *testing.T) {
	longBody := strings.Repeat("x", 500)
	msgs := []llm.Message{
		{Role: "user", Content: "Search for news"},
		{Role: "assistant", Content: `{"action":"tool","name":"search_web","arguments":{"query":"news"}}`},
		{Role: "user", Content: "Tool result: " + longBody},
	}

	condensed := condenseToolResults(msgs)

	// No assistant after the tool result, so it should NOT be condensed.
	if condensed[2].Content != msgs[2].Content {
		t.Errorf("last tool result should be preserved, got length: %d", len(condensed[2].Content))
	}
}

func TestCondenseToolResults_PreservesToolErrors(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "Check email"},
		{Role: "assistant", Content: `{"action":"tool","name":"search_email","arguments":{}}`},
		{Role: "user", Content: "Tool error: connection timeout"},
		{Role: "assistant", Content: `{"action":"respond","text":"Sorry, I couldn't check your email due to a timeout."}`},
	}

	condensed := condenseToolResults(msgs)

	// Tool errors should never be condensed.
	if condensed[2].Content != msgs[2].Content {
		t.Errorf("tool error should be preserved, got: %s", condensed[2].Content)
	}
}

func TestCondenseToolResults_MultipleRoundsLong(t *testing.T) {
	longEmail := "Tool result: " + strings.Repeat("Email from Alice about project X. ", 20)
	longCal := "Tool result: " + strings.Repeat("Meeting with team to discuss goals. ", 20)
	msgs := []llm.Message{
		{Role: "user", Content: "Brief me"},
		{Role: "assistant", Content: `{"action":"tool","name":"search_email","arguments":{}}`},
		{Role: "user", Content: longEmail},
		{Role: "assistant", Content: `{"action":"tool","name":"search_calendar","arguments":{}}`},
		{Role: "user", Content: longCal},
		{Role: "assistant", Content: `{"action":"respond","text":"Here's your briefing."}`},
	}

	condensed := condenseToolResults(msgs)

	// Both intermediate long tool results should be condensed.
	if !strings.Contains(condensed[2].Content, "[search_email]") {
		t.Errorf("first tool result should be condensed, got: %s", condensed[2].Content)
	}
	if !strings.Contains(condensed[4].Content, "[search_calendar]") {
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

func TestCondenseToolResults_EmptySlice(t *testing.T) {
	condensed := condenseToolResults(nil)
	if condensed != nil {
		t.Error("expected nil for nil input")
	}
}

func TestSummarizeToolContext_WithTools(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", Content: `{"action":"tool","name":"search_web","arguments":{"query":"Taylor Swift Fate of Ophelia album"}}`},
		{Role: "user", Content: `Tool result: "The Fate of Ophelia" is a song by Taylor Swift, lead single from The Life of a Showgirl (2025).`},
		{Role: "assistant", Content: `{"action":"respond","text":"It's from The Life of a Showgirl."}`},
	}

	ctx, used := summarizeToolContext(msgs)

	if !used {
		t.Fatal("expected toolsUsed=true")
	}
	if !strings.Contains(ctx, "search_web") {
		t.Error("should contain tool name")
	}
	if !strings.Contains(ctx, "Taylor Swift") {
		t.Error("should contain tool result content")
	}
	if !strings.Contains(ctx, "The Life of a Showgirl") {
		t.Error("should contain full result detail")
	}
}

func TestSummarizeToolContext_NoTools(t *testing.T) {
	msgs := []llm.Message{
		{Role: "assistant", Content: "Hello!"},
	}

	ctx, used := summarizeToolContext(msgs)

	if used {
		t.Error("expected toolsUsed=false")
	}
	if ctx != "" {
		t.Errorf("expected empty context, got: %s", ctx)
	}
}

func TestSummarizeToolContext_PreservesFullResults(t *testing.T) {
	longResult := "Tool result: " + strings.Repeat("important fact. ", 50)
	msgs := []llm.Message{
		{Role: "assistant", Content: `{"action":"tool","name":"search_web","arguments":{}}`},
		{Role: "user", Content: longResult},
		{Role: "assistant", Content: `{"action":"respond","text":"Done."}`},
	}

	ctx, used := summarizeToolContext(msgs)

	if !used {
		t.Fatal("expected toolsUsed=true")
	}
	if !strings.Contains(ctx, "important fact") {
		t.Error("should contain full result content")
	}
}
