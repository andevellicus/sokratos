package tools

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
	OnAccess func() // called on every successful request (VRAM auditor)
	cb       circuitBreaker
	logTag   string // "[dtc]" or "[subagent]"
}

// dtcRequest is the payload sent to /v1/chat/completions.
type dtcRequest struct {
	Model       string       `json:"model,omitempty"`
	Messages    []dtcMessage `json:"messages"`
	Temperature float64      `json:"temperature"`
	MaxTokens   int          `json:"max_tokens"`
	Think       *bool        `json:"think,omitempty"`   // when false, disables chain-of-thought reasoning (llama-server)
	Grammar     string       `json:"grammar,omitempty"` // GBNF grammar constraint (used by SubagentClient)
}

type dtcMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type dtcResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // GLM models put output here even with think:false
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

// thinkFalse is a reusable pointer to false for triage requests.
var thinkFalse = func() *bool { b := false; return &b }()

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

	var raw dtcResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(raw.Choices) == 0 {
		return "", fmt.Errorf("server returned empty choices array")
	}

	// Notify the VRAM auditor that the model was just used, preventing
	// idle eviction between frequent triage/consolidation calls.
	if bc.OnAccess != nil {
		bc.OnAccess()
	}

	// GLM models may put all output into reasoning_content even when
	// think:false is set (template quirk). Fall back to reasoning_content
	// when content is empty.
	content := strings.TrimSpace(raw.Choices[0].Message.Content)
	if content == "" {
		content = strings.TrimSpace(raw.Choices[0].Message.ReasoningContent)
	}
	return content, nil
}

// ensureLoaded checks whether the server has the model loaded by sending a
// minimal 1-token probe. If the probe fails with a transient error (model not
// loaded), it triggers loading and polls /health until ready. For always-on
// dedicated servers, this serves as a lightweight health check (~10ms).
func (bc *baseClient) ensureLoaded(ctx context.Context) error {
	probe, _ := json.Marshal(dtcRequest{
		Model:     bc.Model,
		Messages:  []dtcMessage{{Role: "user", Content: "ping"}},
		MaxTokens: 1,
		Think:     thinkFalse,
	})

	_, err := bc.doRequest(ctx, probe)
	if err == nil {
		return nil // model is loaded and responding
	}

	if !isTransientError(err) {
		return err // genuine error, not a loading issue
	}

	// The probe triggered model loading. Wait for it to finish.
	logger.Log.Infof("%s model %q not loaded, waiting for on-demand load...", bc.logTag, bc.Model)
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
