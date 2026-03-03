package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"sokratos/httputil"
)

// --- Embedding client ---

type embeddingReq struct {
	Input any    `json:"input"` // string or []string for batch
	Model string `json:"model"`
}

type embeddingResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

var embeddingHTTPClient = httputil.NewClient(TimeoutEmbeddingCall)

// embeddedChunk pairs a text fragment with its embedding vector.
type embeddedChunk struct {
	Text      string
	Embedding []float32
}

// doEmbeddingRequest marshals input, sends the HTTP request to the embedding
// endpoint, checks the response status, and decodes the result. Shared by
// GetEmbedding and GetEmbeddings.
func doEmbeddingRequest(ctx context.Context, endpoint, model string, input any) (*embeddingResp, error) {
	body, err := json.Marshal(embeddingReq{Input: input, Model: model})
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := embeddingHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embedding server returned status %d: %s", resp.StatusCode, bytes.TrimSpace(errBody))
	}

	var raw embeddingResp
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}

	return &raw, nil
}

// GetEmbedding calls an OpenAI-compatible /v1/embeddings endpoint and returns the vector.
func GetEmbedding(ctx context.Context, endpoint string, model string, text string) ([]float32, error) {
	raw, err := doEmbeddingRequest(ctx, endpoint, model, text)
	if err != nil {
		return nil, err
	}
	if len(raw.Data) == 0 {
		return nil, fmt.Errorf("embedding server returned empty data array")
	}
	return raw.Data[0].Embedding, nil
}

// embedWithFallback embeds text, recursively splitting in half on "too large"
// errors from the embedding server. Returns one or more (text, embedding)
// pairs. The minimum split size is 100 bytes to prevent infinite recursion.
func embedWithFallback(ctx context.Context, endpoint, model, text string) ([]embeddedChunk, error) {
	emb, err := GetEmbedding(ctx, endpoint, model, text)
	if err != nil {
		if strings.Contains(err.Error(), "too large to process") && len(text) > 100 {
			mid := len(text) / 2
			if nl := strings.LastIndex(text[:mid], "\n"); nl > 0 {
				mid = nl + 1
			}
			left, err := embedWithFallback(ctx, endpoint, model, strings.TrimSpace(text[:mid]))
			if err != nil {
				return nil, err
			}
			right, err := embedWithFallback(ctx, endpoint, model, strings.TrimSpace(text[mid:]))
			if err != nil {
				return nil, err
			}
			return append(left, right...), nil
		}
		return nil, err
	}
	return []embeddedChunk{{Text: text, Embedding: emb}}, nil
}
