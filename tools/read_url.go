package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"

	"sokratos/httputil"
	"sokratos/logger"
)

// Compiled regexes for HTML stripping.
var (
	reScript     = regexp.MustCompile(`(?is)<script.*?</script>`)
	reStyle      = regexp.MustCompile(`(?is)<style.*?</style>`)
	reHTMLTags   = regexp.MustCompile(`<[^>]+>`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

type readURLArgs struct {
	URL      string  `json:"url"`
	MaxChars float64 `json:"max_chars"`
}

// stripHTML removes script/style blocks, HTML tags, decodes entities, and
// collapses whitespace.
func stripHTML(s string) string {
	s = reScript.ReplaceAllString(s, "")
	s = reStyle.ReplaceAllString(s, "")
	s = reHTMLTags.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// NewReadURL returns a ToolFunc that fetches a URL and extracts text content.
func NewReadURL() ToolFunc {
	client := httputil.NewClient(TimeoutURLFetch)

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a readURLArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if strings.TrimSpace(a.URL) == "" {
			return "url is required", nil
		}

		const (
			defaultMaxReadChars = 2000
			maxReadChars        = 10000
		)
		maxChars := defaultMaxReadChars
		if a.MaxChars > 0 {
			maxChars = int(a.MaxChars)
		}
		if maxChars > maxReadChars {
			maxChars = maxReadChars
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return fmt.Sprintf("Failed to create request: %v", err), nil
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Sprintf("Failed to fetch URL: %v", err), nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Sprintf("HTTP %d: failed to fetch URL", resp.StatusCode), nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Sprintf("Failed to read response: %v", err), nil
		}

		text := stripHTML(string(body))

		if len(text) > maxChars {
			text = text[:maxChars] + "\n... (truncated)"
		}

		logger.Log.Infof("[read_url] fetched %d chars from %s", len(text), a.URL)
		return text, nil
	}
}
