package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	nurl "net/url"
	"sort"

	readability "github.com/go-shiori/go-readability"

	"sokratos/logger"
	"sokratos/memory"
)

type readURLArgs struct {
	URL   string `json:"url"`
	Query string `json:"query"`
}

const maxContentLength = 30_000
const maxResultLen = 2000
const chunkSize = 500

// NewReadURL returns a ToolFunc that fetches a URL and returns semantically
// relevant chunks. When embeddings are unavailable or no query is provided,
// it falls back to prefix truncation.
func NewReadURL(embedEndpoint, embedModel string) ToolFunc {
	client := &http.Client{Timeout: TimeoutURLFetch}

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a readURLArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.URL == "" {
			return "error: url is required", nil
		}

		parsed, err := nurl.Parse(a.URL)
		if err != nil {
			return fmt.Sprintf("invalid url: %v", err), nil
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return fmt.Sprintf("failed to create request: %v", err), nil
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Sprintf("request failed: %v", err), nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status), nil
		}

		article, err := readability.FromReader(resp.Body, parsed)
		if err != nil {
			return fmt.Sprintf("failed to extract article: %v", err), nil
		}

		content := article.TextContent
		if len(content) > maxContentLength {
			content = content[:maxContentLength]
		}

		// If content already fits, return as-is.
		if len(content) <= maxResultLen {
			return content, nil
		}

		// Try semantic selection if query and embeddings are available.
		if a.Query != "" && embedEndpoint != "" {
			selected, err := selectRelevantChunks(ctx, content, a.Query, embedEndpoint, embedModel)
			if err != nil {
				logger.Log.Warnf("[read_url] semantic selection failed, falling back to prefix: %v", err)
			} else {
				return selected, nil
			}
		}

		// Fallback: prefix truncation.
		return content[:maxResultLen] + "\n... (truncated)", nil
	}
}

// selectRelevantChunks splits content into chunks, embeds them along with the
// query, and returns the top-k most relevant chunks that fit within the result
// budget, re-sorted by original position for coherent reading order.
func selectRelevantChunks(ctx context.Context, content, query, embedEndpoint, embedModel string) (string, error) {
	chunks := memory.ChunkText(content, chunkSize)
	if len(chunks) == 0 {
		return content, nil
	}

	// Build batch: all chunks + the query as the last element.
	texts := make([]string, len(chunks)+1)
	copy(texts, chunks)
	texts[len(chunks)] = query

	embeddings, err := memory.GetEmbeddings(ctx, embedEndpoint, embedModel, texts)
	if err != nil {
		return "", fmt.Errorf("batch embed: %w", err)
	}

	queryEmb := embeddings[len(chunks)]

	type scored struct {
		index int
		score float32
		text  string
	}

	scored_chunks := make([]scored, len(chunks))
	for i, chunk := range chunks {
		scored_chunks[i] = scored{
			index: i,
			score: cosineSimilarity(embeddings[i], queryEmb),
			text:  chunk,
		}
	}

	// Sort by score descending.
	sort.Slice(scored_chunks, func(i, j int) bool {
		return scored_chunks[i].score > scored_chunks[j].score
	})

	// Select top-k that fit within budget.
	var selected []scored
	budget := maxResultLen
	for _, sc := range scored_chunks {
		needed := len(sc.text)
		if len(selected) > 0 {
			needed += 4 // "\n\n" separator + some margin
		}
		if needed > budget {
			continue
		}
		selected = append(selected, sc)
		budget -= needed
	}

	// Re-sort by original position for coherent reading order.
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].index < selected[j].index
	})

	// Join selected chunks.
	result := ""
	for i, sc := range selected {
		if i > 0 {
			result += "\n\n"
		}
		result += sc.text
	}
	return result, nil
}

// cosineSimilarity computes the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}
