package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"sokratos/httputil"
	"sokratos/logger"
)

type searchWebArgs struct {
	Query      string  `json:"query"`
	MaxResults float64 `json:"max_results"`
}

type searxngResponse struct {
	Results []searxngResult `json:"results"`
}

type searxngResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// NewSearchWeb returns a ToolFunc that searches the web via a SearXNG instance.
func NewSearchWeb(searxngURL string) ToolFunc {
	client := httputil.NewClient(TimeoutSearXNG)

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		a, err := ParseArgs[searchWebArgs](args)
		if err != nil {
			return err.Error(), nil
		}
		if strings.TrimSpace(a.Query) == "" {
			return "query is required", nil
		}

		const (
			defaultSearchResults = 5
			maxSearchResults     = 20
		)
		maxResults := defaultSearchResults
		if a.MaxResults > 0 {
			maxResults = int(a.MaxResults)
		}
		if maxResults > maxSearchResults {
			maxResults = maxSearchResults
		}

		reqURL := fmt.Sprintf("%s/search?q=%s&format=json", searxngURL, url.QueryEscape(a.Query))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return fmt.Sprintf("Failed to create request: %v", err), nil
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Sprintf("Search failed: %v", err), nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Sprintf("Search failed with status %d", resp.StatusCode), nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Sprintf("Failed to read response: %v", err), nil
		}

		var sr searxngResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			return fmt.Sprintf("Failed to parse results: %v", err), nil
		}

		if len(sr.Results) == 0 {
			return "No results found.", nil
		}

		if len(sr.Results) > maxResults {
			sr.Results = sr.Results[:maxResults]
		}

		var b strings.Builder
		for i, r := range sr.Results {
			fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Content)
		}

		logger.Log.Infof("[search_web] %d results for %q", len(sr.Results), a.Query)
		return b.String(), nil
	}
}
