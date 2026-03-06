package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"sokratos/clients"
	"sokratos/httputil"
	"sokratos/logger"
	"sokratos/tokens"
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

const (
	defaultSearchResults = 8
	maxSearchResults     = 20
	autoFetchTop         = 3    // auto-read top N result pages
	fetchMaxChars        = 4000 // per-page cap when auto-fetching
)

// fetchedResult holds an auto-fetched page's content (or error).
type fetchedResult struct {
	index   int
	content string
	err     error
}

// NewSearchWeb returns a ToolFunc that searches the web via a SearXNG instance.
// When sc is non-nil, the top results are auto-fetched in parallel and
// optionally summarized via the subagent for richer, more focused content.
func NewSearchWeb(searxngURL string, sc *clients.SubagentClient) ToolFunc {
	searchClient := httputil.NewClient(TimeoutSearXNG)
	fetchClient := httputil.NewClient(TimeoutURLFetch)

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		a, err := ParseArgs[searchWebArgs](args)
		if err != nil {
			return err.Error(), nil
		}
		if strings.TrimSpace(a.Query) == "" {
			return "query is required", nil
		}

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

		resp, err := searchClient.Do(req)
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

		// Auto-fetch top N result pages in parallel.
		fetchCount := autoFetchTop
		if fetchCount > len(sr.Results) {
			fetchCount = len(sr.Results)
		}
		fetched := make(map[int]string) // index → content
		if fetchCount > 0 {
			results := make(chan fetchedResult, fetchCount)
			var wg sync.WaitGroup
			for i := 0; i < fetchCount; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					text, err := fetchURL(ctx, fetchClient, sr.Results[idx].URL, fetchMaxChars)
					results <- fetchedResult{index: idx, content: text, err: err}
				}(i)
			}
			go func() { wg.Wait(); close(results) }()

			for fr := range results {
				if fr.err != nil {
					logger.Log.Debugf("[search_web] auto-fetch failed for %s: %v", sr.Results[fr.index].URL, fr.err)
					continue
				}
				fetched[fr.index] = fr.content
			}

			// Try subagent summarization for fetched pages (non-blocking).
			if sc != nil && len(fetched) > 0 {
				fetched = trySummarize(ctx, sc, a.Query, sr.Results, fetched)
			}
		}

		// Format output.
		var b strings.Builder
		for i, r := range sr.Results {
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
			if content, ok := fetched[i]; ok {
				fmt.Fprintf(&b, "   %s\n\n", content)
			} else {
				fmt.Fprintf(&b, "   %s\n\n", r.Content)
			}
		}

		logger.Log.Infof("[search_web] %d results for %q (%d auto-fetched)", len(sr.Results), a.Query, len(fetched))
		return b.String(), nil
	}
}

// trySummarize attempts to summarize fetched page content via the subagent.
// Uses TryComplete (non-blocking) — falls back to raw text if no slot available.
// Summarization runs in parallel across available subagent slots.
func trySummarize(ctx context.Context, sc *clients.SubagentClient, query string, results []searxngResult, fetched map[int]string) map[int]string {
	type summaryResult struct {
		index   int
		summary string
	}

	ch := make(chan summaryResult, len(fetched))
	var wg sync.WaitGroup

	systemPrompt := "Extract the key information from this web page relevant to the search query. Be concise — focus on facts, data, and actionable content. Skip navigation, ads, and boilerplate. Return only the extracted content, no preamble."

	for idx, content := range fetched {
		wg.Add(1)
		go func(idx int, content string) {
			defer wg.Done()
			userContent := fmt.Sprintf("Search query: %s\n\nPage: %s\n\n%s", query, results[idx].Title, content)
			summary, err := sc.TryComplete(ctx, systemPrompt, userContent, tokens.WebSummary)
			if err != nil {
				// No slot available or error — keep raw text.
				return
			}
			ch <- summaryResult{index: idx, summary: strings.TrimSpace(summary)}
		}(idx, content)
	}
	go func() { wg.Wait(); close(ch) }()

	for sr := range ch {
		if sr.summary != "" {
			fetched[sr.index] = sr.summary
		}
	}
	return fetched
}
