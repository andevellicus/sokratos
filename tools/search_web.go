package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type searchArgs struct {
	Query string `json:"query"`
}

type searxResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type searxResponse struct {
	Results []searxResult `json:"results"`
}

// NewSearchWeb returns a ToolFunc that queries a SearXNG instance at the given base URL.
func NewSearchWeb(searxngURL string) ToolFunc {
	client := &http.Client{Timeout: TimeoutSearXNG}

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a searchArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Query == "" {
			return "error: query is required", nil
		}

		reqURL := fmt.Sprintf("%s/search?q=%s&format=json", strings.TrimRight(searxngURL, "/"), url.QueryEscape(a.Query))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return fmt.Sprintf("failed to create request: %v", err), nil
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Sprintf("search request failed: %v", err), nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Sprintf("search returned status %d: %s", resp.StatusCode, string(body)), nil
		}

		var sr searxResponse
		if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
			return fmt.Sprintf("failed to parse search results: %v", err), nil
		}

		if len(sr.Results) == 0 {
			return "no results found", nil
		}

		limit := min(5, len(sr.Results))

		var b strings.Builder
		for i, r := range sr.Results[:limit] {
			fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Content)
		}
		return b.String(), nil
	}
}
