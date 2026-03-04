package clients

import (
	"encoding/json"
	"testing"
)

func TestChatMessageMarshalJSON_StringContent(t *testing.T) {
	msg := chatMessage{Role: "user", Content: "hello"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if raw["role"] != "user" {
		t.Errorf("role = %v, want user", raw["role"])
	}
	content, ok := raw["content"].(string)
	if !ok {
		t.Fatalf("content is %T, want string", raw["content"])
	}
	if content != "hello" {
		t.Errorf("content = %q, want hello", content)
	}
}

func TestChatMessageMarshalJSON_MultimodalContent(t *testing.T) {
	msg := chatMessage{
		Role: "user",
		Parts: []contentPart{
			{Type: "text", Text: "Describe this image"},
			{Type: "image_url", ImageURL: &imageURL{URL: "data:image/png;base64,abc123"}},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if raw["role"] != "user" {
		t.Errorf("role = %v, want user", raw["role"])
	}
	parts, ok := raw["content"].([]any)
	if !ok {
		t.Fatalf("content is %T, want array", raw["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}

	// First part: text
	p0 := parts[0].(map[string]any)
	if p0["type"] != "text" {
		t.Errorf("parts[0].type = %v, want text", p0["type"])
	}
	if p0["text"] != "Describe this image" {
		t.Errorf("parts[0].text = %v, want 'Describe this image'", p0["text"])
	}

	// Second part: image_url
	p1 := parts[1].(map[string]any)
	if p1["type"] != "image_url" {
		t.Errorf("parts[1].type = %v, want image_url", p1["type"])
	}
	imgURL := p1["image_url"].(map[string]any)
	if imgURL["url"] != "data:image/png;base64,abc123" {
		t.Errorf("parts[1].image_url.url = %v, want data URI", imgURL["url"])
	}
}

func TestChatMessageMarshalJSON_MixedRequest(t *testing.T) {
	// Simulates a chatRequest with mixed messages: system (string) + user (multimodal)
	msgs := []chatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{
			Role: "user",
			Parts: []contentPart{
				{Type: "text", Text: "What is this?"},
				{Type: "image_url", ImageURL: &imageURL{URL: "data:image/jpeg;base64,xyz"}},
			},
		},
	}

	data, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(raw) != 2 {
		t.Fatalf("len = %d, want 2", len(raw))
	}

	// First message: plain string content
	if _, ok := raw[0]["content"].(string); !ok {
		t.Errorf("msgs[0].content is %T, want string", raw[0]["content"])
	}

	// Second message: array content
	if _, ok := raw[1]["content"].([]any); !ok {
		t.Errorf("msgs[1].content is %T, want array", raw[1]["content"])
	}
}
