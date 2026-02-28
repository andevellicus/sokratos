package main

import (
	"testing"
	"unicode/utf16"
)

// TestMdToEntities_EmojiOffsets verifies that entity offsets are correct when
// emojis (multi-byte UTF-16 surrogate pairs) appear before formatted text.
// This was the root cause of "entity ends in middle of UTF-16 symbol" errors.
func TestMdToEntities_EmojiOffsets(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantText string
		wantEnts []telegramEntity
	}{
		{
			name:     "bold after emoji",
			input:    "📅 **bold**",
			wantText: "📅 bold",
			wantEnts: []telegramEntity{
				{Type: "bold", Offset: 3, Length: 4}, // 📅=2 + space=1 → offset 3
			},
		},
		{
			name:     "italic before bold",
			input:    "*italic* then **bold**",
			wantText: "italic then bold",
			wantEnts: []telegramEntity{
				{Type: "italic", Offset: 0, Length: 6},
				{Type: "bold", Offset: 12, Length: 4}, // "italic then " = 12
			},
		},
		{
			name:     "emoji between italic and bold",
			input:    "*italic* 📅 **bold**",
			wantText: "italic 📅 bold",
			wantEnts: []telegramEntity{
				{Type: "italic", Offset: 0, Length: 6},
				{Type: "bold", Offset: 10, Length: 4}, // "italic " + 📅(2) + " " = 10
			},
		},
		{
			name:     "link after emoji",
			input:    "🎉 [click here](https://example.com)",
			wantText: "🎉 click here",
			wantEnts: []telegramEntity{
				{Type: "text_link", Offset: 3, Length: 10, URL: "https://example.com"},
			},
		},
		{
			name:     "strikethrough after emoji",
			input:    "📊 ~~deleted~~",
			wantText: "📊 deleted",
			wantEnts: []telegramEntity{
				{Type: "strikethrough", Offset: 3, Length: 7},
			},
		},
		{
			name:     "multiple formats mixed order",
			input:    "**bold** and *italic* done",
			wantText: "bold and italic done",
			wantEnts: []telegramEntity{
				{Type: "bold", Offset: 0, Length: 4},
				{Type: "italic", Offset: 9, Length: 6}, // "bold and " = 9
			},
		},
		{
			name:     "no formatting",
			input:    "plain text 📅",
			wantText: "plain text 📅",
			wantEnts: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ents := mdToEntities(tt.input)
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if len(ents) != len(tt.wantEnts) {
				t.Fatalf("got %d entities, want %d: %+v", len(ents), len(tt.wantEnts), ents)
			}
			for i, got := range ents {
				want := tt.wantEnts[i]
				if got.Type != want.Type || got.Offset != want.Offset || got.Length != want.Length || got.URL != want.URL {
					t.Errorf("entity[%d] = %+v, want %+v", i, got, want)
				}
			}
			// Verify no entity lands mid-surrogate by checking offset+length doesn't exceed total.
			totalLen := len(utf16.Encode([]rune(text)))
			for i, e := range ents {
				if e.Offset+e.Length > totalLen {
					t.Errorf("entity[%d] extends beyond text: offset=%d length=%d total=%d", i, e.Offset, e.Length, totalLen)
				}
			}
		})
	}
}
