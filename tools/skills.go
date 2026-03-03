package tools

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

	"github.com/BurntSushi/toml"
	"github.com/dop251/goja"
	"github.com/jackc/pgx/v5/pgxpool"

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
	Dir      string // skill directory (for config.txt loading)
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
			Dir:      skillDir,
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
// the skill in the tool registry. The handler.js source is read from disk on
// each invocation so edits take effect immediately without a restart.
func RegisterSkill(registry *Registry, skill Skill, pool *pgxpool.Pool) {
	name := skill.Manifest.Name
	source := skill.Source // fallback if dir is empty (e.g. create_skill test)
	dir := skill.Dir

	fn := func(ctx context.Context, args json.RawMessage) (string, error) {
		currentSource := source
		if dir != "" {
			if data, err := os.ReadFile(filepath.Join(dir, "scripts", "handler.js")); err == nil {
				currentSource = string(data)
			}
		}
		return ExecuteSkill(ctx, name, currentSource, dir, args, pool)
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
// as a string. If dir is non-empty and contains a config.toml, its contents are
// injected as the skill_config global object (read fresh each call). The pool
// parameter enables the kv_get/kv_set/kv_delete per-skill key-value store; pass
// nil to disable KV (functions will throw a TypeError).
func ExecuteSkill(ctx context.Context, name, source, dir string, args json.RawMessage, pool *pgxpool.Pool) (string, error) {
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

	// Inject skill_config from config.toml (read fresh each call so edits take
	// effect immediately). Parsed as TOML into a map and injected as a JS object.
	// Falls back to config.txt as a raw string for backward compatibility.
	var skillConfig any
	if dir != "" {
		if data, err := os.ReadFile(filepath.Join(dir, "config.toml")); err == nil {
			var parsed map[string]any
			if err := toml.Unmarshal(data, &parsed); err != nil {
				logger.Log.Warnf("[skills] %s: invalid config.toml: %v", name, err)
				skillConfig = map[string]any{}
			} else {
				skillConfig = parsed
			}
		} else if data, err := os.ReadFile(filepath.Join(dir, "config.txt")); err == nil {
			// Legacy fallback: inject as raw string.
			skillConfig = string(data)
		} else {
			skillConfig = map[string]any{}
		}
	} else {
		skillConfig = map[string]any{}
	}
	vm.Set("skill_config", skillConfig)

	// Bind the HTTP bridge.
	vm.Set("http_request", func(call goja.FunctionCall) goja.Value {
		return httpBridge(vm, call)
	})

	// --- console.log / console.warn / console.error ---
	var logBuf []string
	consoleObj := vm.NewObject()
	for _, level := range []string{"log", "warn", "error"} {
		lvl := level
		consoleObj.Set(lvl, func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for i, arg := range call.Arguments {
				parts[i] = arg.String()
			}
			logBuf = append(logBuf, fmt.Sprintf("[%s] %s", strings.ToUpper(lvl), strings.Join(parts, " ")))
			return goja.Undefined()
		})
	}
	vm.Set("console", consoleObj)

	// --- btoa / atob (base64) ---
	vm.Set("btoa", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			panic(vm.NewTypeError("btoa requires 1 argument"))
		}
		return vm.ToValue(base64.StdEncoding.EncodeToString([]byte(call.Arguments[0].String())))
	})
	vm.Set("atob", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			panic(vm.NewTypeError("atob requires 1 argument"))
		}
		decoded, err := base64.StdEncoding.DecodeString(call.Arguments[0].String())
		if err != nil {
			panic(vm.NewTypeError("atob: invalid base64: " + err.Error()))
		}
		return vm.ToValue(string(decoded))
	})

	// --- sleep(ms) ---
	vm.Set("sleep", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			return goja.Undefined()
		}
		ms := call.Arguments[0].ToInteger()
		if ms <= 0 {
			return goja.Undefined()
		}
		dur := time.Duration(ms) * time.Millisecond
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); dur > remaining {
				dur = remaining
			}
		}
		if dur > 5*time.Second {
			dur = 5 * time.Second
		}
		time.Sleep(dur)
		if ctx.Err() != nil {
			panic(vm.NewTypeError("context cancelled during sleep"))
		}
		return goja.Undefined()
	})

	// --- env(key) — reads SKILL_<key> env vars only ---
	vm.Set("env", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			panic(vm.NewTypeError("env requires 1 argument"))
		}
		val := os.Getenv("SKILL_" + call.Arguments[0].String())
		if val == "" {
			return goja.Undefined()
		}
		return vm.ToValue(val)
	})

	// --- hash_sha256 / hash_hmac_sha256 ---
	vm.Set("hash_sha256", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			panic(vm.NewTypeError("hash_sha256 requires 1 argument"))
		}
		h := sha256.Sum256([]byte(call.Arguments[0].String()))
		return vm.ToValue(hex.EncodeToString(h[:]))
	})
	vm.Set("hash_hmac_sha256", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 2 {
			panic(vm.NewTypeError("hash_hmac_sha256 requires 2 arguments: key, message"))
		}
		mac := hmac.New(sha256.New, []byte(call.Arguments[0].String()))
		mac.Write([]byte(call.Arguments[1].String()))
		return vm.ToValue(hex.EncodeToString(mac.Sum(nil)))
	})

	// --- kv_get / kv_set / kv_delete (per-skill PostgreSQL KV store) ---
	if pool != nil {
		vm.Set("kv_get", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("kv_get requires 1 argument: key"))
			}
			key := call.Arguments[0].String()
			kvCtx, cancel := context.WithTimeout(ctx, TimeoutSkillKV)
			defer cancel()
			var val string
			err := pool.QueryRow(kvCtx,
				"SELECT value FROM skill_kv WHERE skill_name=$1 AND key=$2", name, key).Scan(&val)
			if err != nil {
				return goja.Undefined()
			}
			return vm.ToValue(val)
		})
		vm.Set("kv_set", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("kv_set requires 2 arguments: key, value"))
			}
			key := call.Arguments[0].String()
			value := call.Arguments[1].String()
			kvCtx, cancel := context.WithTimeout(ctx, TimeoutSkillKV)
			defer cancel()
			_, err := pool.Exec(kvCtx,
				`INSERT INTO skill_kv (skill_name, key, value, updated_at) VALUES ($1, $2, $3, now())
				 ON CONFLICT (skill_name, key) DO UPDATE SET value=$3, updated_at=now()`,
				name, key, value)
			if err != nil {
				panic(vm.NewTypeError("kv_set failed: " + err.Error()))
			}
			return goja.Undefined()
		})
		vm.Set("kv_delete", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("kv_delete requires 1 argument: key"))
			}
			key := call.Arguments[0].String()
			kvCtx, cancel := context.WithTimeout(ctx, TimeoutSkillKV)
			defer cancel()
			_, err := pool.Exec(kvCtx,
				"DELETE FROM skill_kv WHERE skill_name=$1 AND key=$2", name, key)
			if err != nil {
				panic(vm.NewTypeError("kv_delete failed: " + err.Error()))
			}
			return goja.Undefined()
		})
	} else {
		kvUnavailable := func(call goja.FunctionCall) goja.Value {
			panic(vm.NewTypeError("kv store unavailable: no database connection"))
		}
		vm.Set("kv_get", kvUnavailable)
		vm.Set("kv_set", kvUnavailable)
		vm.Set("kv_delete", kvUnavailable)
	}

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
			logger.Log.Debugf("[skills] waiting for pending promise in %s", name)
			// goja doesn't support event loops without an extension, but our http_request
			// bridge is fully synchronous, so the promise should already be fulfilled
			// or rejected by the time RunString completes.
			return fmt.Sprintf("Skill %q execution error: Async operations with true event-loop blocking are not supported in this sandbox.", name), nil
		}
	}

	// Build result string.
	var resultStr string
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		resultStr = ""
	} else {
		exported := val.Export()
		switch exported.(type) {
		case map[string]interface{}, []interface{}:
			b, err := json.Marshal(exported)
			if err != nil {
				resultStr = fmt.Sprintf("Skill %q returned non-serializable object: %v", name, err)
			} else {
				resultStr = string(b)
			}
		default:
			resultStr = val.String()
		}
	}

	// Append console log buffer if non-empty.
	if len(logBuf) > 0 {
		resultStr += "\n---\n" + strings.Join(logBuf, "\n")
	}

	return resultStr, nil
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

// SyncSkills scans the skills directory, compares against the registry, and
// handles new, removed, and changed skills. It tracks SKILL.md mtimes in the
// provided cache to detect schema changes. Returns true if any changes were
// applied (and the grammar was rebuilt).
func SyncSkills(registry *Registry, skillsDir string, rebuildGrammar GrammarRebuildFunc, mtimeCache map[string]time.Time, pool *pgxpool.Pool) bool {
	diskSkills, err := LoadSkills(skillsDir)
	if err != nil {
		logger.Log.Warnf("[skills] hot-reload: failed to load skills: %v", err)
		return false
	}

	// Index disk skills by name.
	onDisk := make(map[string]Skill, len(diskSkills))
	for _, s := range diskSkills {
		onDisk[s.Manifest.Name] = s
	}

	// Index registered skills by name.
	registered := make(map[string]struct{})
	for _, s := range registry.Schemas() {
		if s.IsSkill {
			registered[s.Name] = struct{}{}
		}
	}

	var added, removed, updated []string

	// Detect new and updated skills.
	for name, skill := range onDisk {
		mdPath := filepath.Join(skill.Dir, "SKILL.md")
		info, err := os.Stat(mdPath)
		if err != nil {
			continue
		}
		diskMtime := info.ModTime()

		if _, ok := registered[name]; !ok {
			// New skill on disk, not in registry.
			RegisterSkill(registry, skill, pool)
			mtimeCache[name] = diskMtime
			added = append(added, name)
		} else if cached, ok := mtimeCache[name]; !ok {
			// First sync after startup — just populate the cache.
			mtimeCache[name] = diskMtime
		} else if diskMtime.After(cached) {
			// SKILL.md changed — re-register.
			registry.Unregister(name)
			RegisterSkill(registry, skill, pool)
			mtimeCache[name] = diskMtime
			updated = append(updated, name)
		}
	}

	// Detect removed skills.
	for name := range registered {
		if _, ok := onDisk[name]; !ok {
			registry.Unregister(name)
			delete(mtimeCache, name)
			removed = append(removed, name)
		}
	}

	changed := len(added) + len(removed) + len(updated)
	if changed == 0 {
		return false
	}

	rebuildGrammar()
	logger.Log.Infof("[skills] hot-reload: added %v, removed %v, updated %v", added, removed, updated)
	return true
}
