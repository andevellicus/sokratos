package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sokratos/logger"
)

// baseClient holds the shared HTTP client, URL, model, and circuit breaker
// for LLM server interactions (DTC, subagent, and on-demand router).
type baseClient struct {
	URL      string
	Model    string
	client   *http.Client
	cb     circuitBreaker
	logTag   string // "[dtc]" or "[subagent]"
}

// chatRequest is the payload sent to /v1/chat/completions (shared by DTC and SubagentClient).
type chatRequest struct {
	Model              string         `json:"model,omitempty"`
	Messages           []chatMessage   `json:"messages"`
	Temperature        float64        `json:"temperature"`
	MaxTokens          int            `json:"max_tokens"`
	Think              *bool          `json:"think,omitempty"`                // when false, disables chain-of-thought reasoning (llama-server)
	Grammar            string         `json:"grammar,omitempty"`              // GBNF grammar constraint (used by SubagentClient)
	ReasoningFormat    string         `json:"reasoning_format,omitempty"`     // "deepseek" → split <think> into reasoning_content vs content
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"` // Jinja template vars (e.g. enable_thinking for Qwen3.5)
}

// contentPart represents one element in the OpenAI vision content array.
// Local to the clients package to avoid importing llm/.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

// imageURL carries an image data-URI or URL for the vision API.
type imageURL struct {
	URL string `json:"url"`
}

// chatMessage represents a single chat message. When Parts is set (e.g. for
// vision messages), the JSON content field is serialized as an array of
// content parts; otherwise it is a plain string.
type chatMessage struct {
	Role    string        `json:"-"`
	Content string        `json:"-"`
	Parts   []contentPart `json:"-"`
}

// MarshalJSON serializes chatMessage. If Parts is non-empty, content is an
// array of content parts (OpenAI vision format); otherwise it is a plain string.
func (m chatMessage) MarshalJSON() ([]byte, error) {
	if len(m.Parts) > 0 {
		type wire struct {
			Role    string        `json:"role"`
			Content []contentPart `json:"content"`
		}
		return json.Marshal(wire{Role: m.Role, Content: m.Parts})
	}
	type wire struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	return json.Marshal(wire{Role: m.Role, Content: m.Content})
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // thinking output when reasoning_format=deepseek
		} `json:"message"`
	} `json:"choices"`
}

// serverError captures both the HTTP status and the response body from the server.
type serverError struct {
	status int
	detail string
}

func (e *serverError) Error() string {
	return fmt.Sprintf("server returned status %d: %s", e.status, e.detail)
}

// isTransientError returns true for HTTP 500 and 503 errors that may resolve on retry
// (e.g. model loading, slot temporarily unavailable).
func isTransientError(err error) bool {
	if se, ok := err.(*serverError); ok {
		return se.status == 500 || se.status == 503
	}
	return false
}

// doRequest sends a single HTTP request and returns the content or an error.
func (bc *baseClient) doRequest(ctx context.Context, body []byte) (string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, bc.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := bc.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		detail := strings.TrimSpace(string(errBody))
		if detail == "" {
			detail = "no response body"
		}
		return "", &serverError{status: resp.StatusCode, detail: detail}
	}

	var raw chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(raw.Choices) == 0 {
		return "", fmt.Errorf("server returned empty choices array")
	}

	// When reasoning_format=deepseek is set, thinking goes into
	// reasoning_content and the answer goes into content. Fall back to
	// reasoning_content when content is empty (defensive, shouldn't happen
	// in normal operation with Qwen3.5's chat_template_kwargs).
	content := strings.TrimSpace(raw.Choices[0].Message.Content)
	if content == "" {
		content = strings.TrimSpace(raw.Choices[0].Message.ReasoningContent)
	}
	return content, nil
}

// ensureLoaded checks whether the server has the model loaded by hitting
// /health. Unlike the old 1-token inference probe, this doesn't consume
// a slot — so it won't return 503 when all slots are busy.
func (bc *baseClient) ensureLoaded(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bc.URL+"/health", nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	resp, err := bc.client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil // model loaded and healthy
	}

	// Non-200 means model is loading or server is starting up.
	logger.Log.Infof("%s model %q not loaded (health status %d), waiting...", bc.logTag, bc.Model, resp.StatusCode)
	return bc.waitForReady(ctx)
}

// waitForReady polls the server's /health endpoint until it returns 200 or the
// context deadline is reached. Used after a transient error to wait for model
// loading before retrying the real request.
func (bc *baseClient) waitForReady(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	healthURL := bc.URL + "/health"
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
			if err != nil {
				continue
			}
			resp, err := bc.client.Do(req)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				logger.Log.Infof("%s model ready, retrying request", bc.logTag)
				return nil
			}
			logger.Log.Debugf("%s waiting for model load (health status: %d)", bc.logTag, resp.StatusCode)
		}
	}
}
