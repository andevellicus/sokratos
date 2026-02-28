package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dop251/goja"

	"sokratos/httputil"
	"sokratos/logger"
)

// AllowedInternalHosts is a list of host or host:port strings that bypass the
// private/loopback IP check in validateURL. Populated at startup from configured
// service URLs (e.g. SearXNG, embedding endpoint).
var AllowedInternalHosts []string

// SkillManifest holds the parsed SKILL.md frontmatter fields.
type SkillManifest struct {
	Name        string
	Description string
}

// Skill represents a fully loaded skill ready for registration.
type Skill struct {
	Manifest SkillManifest
	Params   []ParamSchema
	Source   string // handler.js content
}

// LoadSkills discovers and loads all skills from the given directory.
// Each skill lives in a subdirectory containing SKILL.md and scripts/handler.js.
// Returns nil if the directory doesn't exist. Logs warnings for malformed skills
// and continues loading others.
func LoadSkills(dir string) ([]Skill, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}

	pattern := filepath.Join(dir, "*", "SKILL.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob skills: %w", err)
	}

	var skills []Skill
	for _, mdPath := range matches {
		skillDir := filepath.Dir(mdPath)
		skillName := filepath.Base(skillDir)

		mdData, err := os.ReadFile(mdPath)
		if err != nil {
			logger.Log.Warnf("[skills] failed to read %s: %v", mdPath, err)
			continue
		}

		manifest, params, err := parseSkillMD(mdData)
		if err != nil {
			logger.Log.Warnf("[skills] failed to parse %s: %v", mdPath, err)
			continue
		}

		// Validate name matches directory.
		if manifest.Name != skillName {
			logger.Log.Warnf("[skills] name mismatch in %s: manifest=%q dir=%q", mdPath, manifest.Name, skillName)
			continue
		}

		handlerPath := filepath.Join(skillDir, "scripts", "handler.js")
		source, err := os.ReadFile(handlerPath)
		if err != nil {
			logger.Log.Warnf("[skills] missing handler.js for %s: %v", skillName, err)
			continue
		}

		if err := ValidateSkillSource(string(source)); err != nil {
			logger.Log.Warnf("[skills] invalid JS in %s: %v", skillName, err)
			continue
		}

		skills = append(skills, Skill{
			Manifest: manifest,
			Params:   params,
			Source:   string(source),
		})
		logger.Log.Infof("[skills] loaded skill: %s", skillName)
	}

	return skills, nil
}

// parseSkillMD parses a SKILL.md file into manifest fields and parameter schemas.
// It extracts YAML frontmatter (name, description) and the ## Parameters markdown table.
func parseSkillMD(data []byte) (SkillManifest, []ParamSchema, error) {
	content := string(data)

	// Extract frontmatter between --- delimiters.
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return SkillManifest{}, nil, fmt.Errorf("missing YAML frontmatter delimiters")
	}
	frontmatter := parts[1]
	body := parts[2]

	manifest, err := parseFrontmatter(frontmatter)
	if err != nil {
		return SkillManifest{}, nil, err
	}

	params := parseParamsTable(body)

	return manifest, params, nil
}

// parseFrontmatter extracts name and description from YAML-like frontmatter.
// Supports single-line values and multi-line description with `|` block scalar.
func parseFrontmatter(fm string) (SkillManifest, error) {
	var m SkillManifest
	lines := strings.Split(fm, "\n")

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if after, ok := strings.CutPrefix(trimmed, "name:"); ok {
			m.Name = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(trimmed, "description:"); ok {
			value := strings.TrimSpace(after)
			if value == "|" {
				// Multi-line block scalar: collect indented lines.
				var descLines []string
				for i+1 < len(lines) {
					next := lines[i+1]
					if len(next) == 0 || next[0] == ' ' || next[0] == '\t' {
						descLines = append(descLines, strings.TrimSpace(next))
						i++
					} else {
						break
					}
				}
				m.Description = strings.TrimSpace(strings.Join(descLines, " "))
			} else {
				m.Description = value
			}
		}
	}

	if m.Name == "" {
		return m, fmt.Errorf("missing required field: name")
	}
	if m.Description == "" {
		return m, fmt.Errorf("missing required field: description")
	}

	return m, nil
}

// parseParamsTable finds a ## Parameters heading and parses the markdown table
// beneath it into ParamSchema slices.
func parseParamsTable(body string) []ParamSchema {
	lines := strings.Split(body, "\n")

	// Find ## Parameters heading.
	paramIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Parameters" {
			paramIdx = i
			break
		}
	}
	if paramIdx < 0 {
		return nil
	}

	// Skip heading, header row, and separator row, then parse data rows.
	var params []ParamSchema
	started := false
	for i := paramIdx + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		// Stop at next heading.
		if strings.HasPrefix(line, "##") {
			break
		}

		// Skip the header row and separator row.
		if !started {
			if strings.Contains(line, "---") {
				started = true
			}
			continue
		}

		// Parse table row: | Name | Type | Required |
		cells := strings.Split(line, "|")
		var cleaned []string
		for _, c := range cells {
			c = strings.TrimSpace(c)
			if c != "" {
				cleaned = append(cleaned, c)
			}
		}
		if len(cleaned) < 3 {
			continue
		}

		name := cleaned[0]
		typ := strings.ToLower(cleaned[1])
		req := strings.ToLower(cleaned[2]) == "yes"

		// Normalize type to our supported set.
		switch typ {
		case "string", "number", "boolean", "array":
			// ok
		default:
			typ = "string"
		}

		params = append(params, ParamSchema{
			Name:     name,
			Type:     typ,
			Required: req,
		})
	}

	return params
}

// RegisterSkill creates a ToolFunc closure wrapping ExecuteSkill and registers
// the skill in the tool registry.
func RegisterSkill(registry *Registry, skill Skill) {
	name := skill.Manifest.Name
	source := skill.Source

	fn := func(ctx context.Context, args json.RawMessage) (string, error) {
		return ExecuteSkill(ctx, name, source, args)
	}

	schema := ToolSchema{
		Name:        name,
		Params:      skill.Params,
		Description: skill.Manifest.Description,
		IsSkill:     true,
	}

	registry.Register(name, fn, schema)
}

// ExecuteSkill creates a fresh goja runtime, injects args and the HTTP bridge,
// and executes the skill's JavaScript source. Returns the last expression value
// as a string.
func ExecuteSkill(ctx context.Context, name, source string, args json.RawMessage) (string, error) {
	vm := goja.New()

	// Inject args as a global object.
	var argsObj any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argsObj); err != nil {
			return fmt.Sprintf("Failed to parse skill arguments: %v", err), nil
		}
	}
	if argsObj == nil {
		argsObj = map[string]any{}
	}
	vm.Set("args", argsObj)

	// Bind the HTTP bridge.
	vm.Set("http_request", func(call goja.FunctionCall) goja.Value {
		return httpBridge(vm, call)
	})

	// Set up timeout via interrupt.
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(TimeoutSkillExec):
			vm.Interrupt("skill execution timeout")
		case <-ctx.Done():
			vm.Interrupt("context cancelled")
		case <-done:
		}
	}()
	defer close(done)

	// Wrap in IIFE if the source contains top-level return statements.
	execSource := source
	if needsIIFEWrap(source) {
		execSource = wrapInIIFE(source)
	}

	val, err := vm.RunString(execSource)
	if err != nil {
		return fmt.Sprintf("Skill %q execution error: %v", name, err), nil
	}

	// If the script was wrapped in an async IIFE, the result is a Promise.
	// We need to resolve it synchronously for the Go caller.
	if promise, ok := val.Export().(*goja.Promise); ok {
		switch promise.State() {
		case goja.PromiseStateFulfilled:
			val = promise.Result()
		case goja.PromiseStateRejected:
			return fmt.Sprintf("Skill %q execution error (Promise rejected): %v", name, promise.Result()), nil
		case goja.PromiseStatePending:
			// Await pending promise
			fmt.Printf("[skills] waiting for pending promise in %s\n", name)
			// goja doesn't support event loops without an extension, but our http_request
			// bridge is fully synchronous, so the promise should already be fulfilled
			// or rejected by the time RunString completes.
			return fmt.Sprintf("Skill %q execution error: Async operations with true event-loop blocking are not supported in this sandbox.", name), nil
		}
	}

	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return "", nil
	}

	// If the returned value is an object or array, JSON-marshal it so
	// the caller gets useful data instead of "[object Object]".
	exported := val.Export()
	switch exported.(type) {
	case map[string]interface{}, []interface{}:
		b, err := json.Marshal(exported)
		if err != nil {
			return fmt.Sprintf("Skill %q returned non-serializable object: %v", name, err), nil
		}
		return string(b), nil
	default:
		return val.String(), nil
	}
}

// httpBridge implements the JS http_request(method, url, headers, body) function.
// Returns an object with {status, body, headers} fields.
func httpBridge(vm *goja.Runtime, call goja.FunctionCall) goja.Value {
	if len(call.Arguments) < 2 {
		panic(vm.NewTypeError("http_request requires at least 2 arguments: method, url"))
	}

	method := call.Arguments[0].String()
	rawURL := call.Arguments[1].String()

	// Validate URL to block private networks.
	if err := validateURL(rawURL); err != nil {
		panic(vm.NewTypeError("http_request blocked: %s", err.Error()))
	}

	var bodyStr string
	if len(call.Arguments) > 3 {
		bodyStr = call.Arguments[3].String()
	}

	// Build HTTP request.
	var bodyReader io.Reader
	if bodyStr != "" {
		bodyReader = strings.NewReader(bodyStr)
	}

	req, err := http.NewRequest(method, rawURL, bodyReader)
	if err != nil {
		panic(vm.NewTypeError("http_request: invalid request: %s", err.Error()))
	}

	// Parse headers object.
	if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
		headersObj := call.Arguments[2].Export()
		if headers, ok := headersObj.(map[string]any); ok {
			for k, v := range headers {
				req.Header.Set(k, fmt.Sprint(v))
			}
		}
	}

	client := httputil.NewClient(TimeoutSkillHTTP)
	resp, err := client.Do(req)
	if err != nil {
		panic(vm.NewTypeError("http_request failed: %s", err.Error()))
	}
	defer resp.Body.Close()

	// Read body with 1MB limit.
	limited := io.LimitReader(resp.Body, 1<<20)
	respBody, err := io.ReadAll(limited)
	if err != nil {
		panic(vm.NewTypeError("http_request: failed to read response: %s", err.Error()))
	}

	// Build response headers map.
	respHeaders := make(map[string]any)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	result := vm.NewObject()
	result.Set("status", resp.StatusCode)
	result.Set("body", string(respBody))
	result.Set("headers", respHeaders)

	return result
}

// validateURL checks that a URL is safe to request: only http/https, no private
// network addresses (RFC1918, loopback, link-local). Hosts listed in
// AllowedInternalHosts bypass the IP check.
func validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}

	// Check allowlist before DNS resolution.
	if isAllowedHost(u.Host, u.Hostname()) {
		return nil
	}

	host := u.Hostname()
	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("blocked private/loopback address: %s", ipStr)
		}
	}

	return nil
}

// isAllowedHost checks if the given host or host:port matches an entry in
// AllowedInternalHosts.
func isAllowedHost(hostPort, hostname string) bool {
	for _, allowed := range AllowedInternalHosts {
		if hostPort == allowed || hostname == allowed {
			return true
		}
	}
	return false
}

// wrapInIIFE wraps source code in an immediately-invoked function expression,
// allowing top-level return statements and top-level await to work in goja script mode.
// We make the IIFE async to natively support await inside skills.
// We also add an extra newline before the closing brace to protect against
// the entire script being a single line ending in a // comment.
func wrapInIIFE(source string) string {
	return "(async function(){\n" + source + "\n\n})()"
}

// needsIIFEWrap tries compiling source and returns true if compilation fails
// with a top-level return or top-level await error (illegal in script mode).
func needsIIFEWrap(source string) bool {
	_, err := goja.Compile("handler.js", source, false)
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "return") || strings.Contains(errStr, "await") || strings.Contains(errStr, "Unexpected identifier")
}

// ValidateSkillSource compiles JavaScript source in goja to catch syntax errors
// without executing it. If the source contains top-level return or await statements
// (illegal in script mode), it retries after wrapping in an async IIFE.
func ValidateSkillSource(source string) error {
	_, err := goja.Compile("handler.js", source, false)
	if err == nil {
		return nil
	}
	// If the error might be solved by a function wrapper, try wrapping in IIFE.
	errStr := err.Error()
	if strings.Contains(errStr, "return") || strings.Contains(errStr, "await") || strings.Contains(errStr, "Unexpected identifier") {
		if _, err2 := goja.Compile("handler.js", wrapInIIFE(source), false); err2 == nil {
			return nil
		}
	}
	return fmt.Errorf("JS syntax error: %w", err)
}

// GenerateSkillMD produces a SKILL.md file content from the given fields.
func GenerateSkillMD(name, description string, params []ParamSchema) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	if strings.Contains(description, "\n") {
		b.WriteString("description: |\n")
		for _, line := range strings.Split(description, "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	} else {
		fmt.Fprintf(&b, "description: %s\n", description)
	}
	b.WriteString("---\n")

	if len(params) > 0 {
		b.WriteString("\n## Parameters\n\n")
		b.WriteString("| Name | Type | Required |\n")
		b.WriteString("|------|------|----------|\n")
		for _, p := range params {
			req := "no"
			if p.Required {
				req = "yes"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", p.Name, p.Type, req)
		}
	}

	return b.String()
}
