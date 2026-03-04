# Skill System

Skills are user-created tools that persist to disk, auto-load on startup, and integrate with the tool registry and grammar system. They run in a sandboxed ES2020 environment (goja) with HTTP bridging, key-value storage, and cryptographic helpers. Skills can be written in JavaScript or TypeScript (transpiled via esbuild).

---

## Disk Layout

```
skills/
  <name>/
    SKILL.md              # Frontmatter manifest (name, description, language, parameters)
    scripts/handler.ts    # TypeScript source (preferred)
    scripts/handler.js    # JavaScript source (fallback if no .ts)
    config.toml           # Optional per-skill configuration (parsed as JS object)
```

If both `handler.ts` and `handler.js` exist, `.ts` takes priority. TypeScript is transpiled to JavaScript at load time via esbuild (pure Go, <1ms).

### SKILL.md Format

```markdown
---
name: get_weather
language: typescript
description: |
  Fetch current weather and forecast for a configured location.
  Returns JSON with current conditions and 3-day forecast.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| location | string | no |
```

The YAML frontmatter defines `name`, `description`, optionally `language` (`"javascript"` default, `"typescript"`), and documents parameters. The description is used in the tool's schema and in dynamic tool descriptions injected into the orchestrator's prompt.

### config.toml

Parsed via TOML into a JS object and injected as the `skill_config` global. Read fresh on each invocation (no restart needed). Falls back to `config.txt` as a raw string if TOML parsing fails.

```toml
location = "New York, NY"
units = "imperial"
```

Accessed in JS/TS as `skill_config.location`, `skill_config.units`, etc.

---

## Runtime Environment

Skills execute in a **goja** ES2020 sandbox with a 30-second timeout (5 minutes with delegation deps). The following globals are available:

| Global | Description |
|--------|-------------|
| `args` | Parsed JSON parameters from the tool invocation |
| `skill_config` | Parsed `config.toml` as a JS object |
| `http_request(method, url, headers, body)` | Synchronous HTTP bridge (15s timeout, 1MB response cap) |
| `console.log(...)` / `console.warn(...)` / `console.error(...)` | Output appended as `[LOG]`/`[WARN]`/`[ERROR]` lines |
| `btoa(s)` / `atob(s)` | Base64 encode/decode |
| `sleep(ms)` | Synchronous sleep (capped at 5s per call) |
| `env(key)` | Read `SKILL_<key>` environment variable (only `SKILL_*` prefix) |
| `kv_get(key)` / `kv_set(key, value)` / `kv_delete(key)` | Per-skill PostgreSQL key-value store |
| `hash_sha256(s)` | SHA-256 hex digest |
| `hash_hmac_sha256(key, msg)` | HMAC-SHA256 hex digest |
| `call_tool(name, args)` | Synchronous tool invocation (self-call prevented) |
| `delegate(directive, context)` | Single subagent dispatch (60s timeout) |
| `delegate_batch(tasks)` | Parallel fan-out (3min timeout) |

### TypeScript Support

TypeScript skills use `declare` statements for sandbox globals (type-only, stripped at transpile time):

```typescript
declare const args: { location?: string };
declare const skill_config: { location?: string } | undefined;
declare function http_request(method: string, url: string, headers: Record<string, string>, body: string): { status: number; body: string };
```

esbuild transpiles TS to ES2020 JS. Type annotations, interfaces, and enums are stripped; the resulting JS runs in goja identically to hand-written JS.

### HTTP Bridge

`http_request(method, url, headers, body)` returns `{status, body, headers}`. Private/internal IPs are blocked by default unless the host appears in `AllowedInternalHosts` (populated from configured service URLs like SearXNG, RSSHub, etc.).

### Key-Value Store

The KV store is namespaced per skill in the `skill_kv` PostgreSQL table. Useful for dedup tracking (seen URLs), rate-limit backoff state, caching, etc. Values are strings — use `JSON.stringify`/`JSON.parse` for structured data.

---

## Lifecycle

### Creating Skills

Skills can be created two ways:

1. **`create_skill` tool** (runtime) — The orchestrator generates the code, validates syntax (transpiles if TS), runs a test execution with `test_args`, writes to disk, registers in the live registry, and rebuilds tool descriptions. Pass `"language": "typescript"` for TS skills.

2. **Manual** — Create the directory structure by hand, then `/reload` or restart to pick it up.

### Hot-Reload

On each heartbeat tick, `SyncSkills()` checks mtimes of skill directories. Changed files trigger re-registration and grammar rebuild. TypeScript files are re-transpiled on each invocation so edits take effect immediately. Also triggered by `/reload`.

### Deletion

Via `manage_skills` tool (`action: "delete"`) or manual removal of the directory.

---

## Using Skills in Routines

Skills are actions like any other — they can be called from routines via the `action` or `actions` field:

```toml
[feed-digest]
interval = "4 hours"
action = "scan_feeds"
goal = "Send a digest of the top items."
silent_if_empty = true
```

Action arguments can be passed via `action_args` with template expansion:

```toml
[feed-digest.action_args.scan_feeds]
count = 5
feed = "hackernews"
```

See [routines.md](routines.md) for the full template syntax.

---

## Naming Convention

Skill names use **snake_case** to match built-in tools (`search_web`, `search_memory`, etc.). The regex allows `[a-z][a-z0-9_-]{1,48}` but snake_case is preferred for consistency.

---

## Built-in Skills

| Skill | Description | Key Config |
|-------|-------------|------------|
| `get_weather` | Current conditions + 3-day forecast via Open-Meteo | `location` |
| `scan_feeds` | RSS/Atom aggregator with parallel article summarization | `[[category]]` with `type`, `route`/`subreddit`/`lists`/`url` |

### scan_feeds Feed Types

| Type | Source | Config Fields |
|------|--------|--------------|
| `twitter` | RSSHub `/twitter/list/` or `/twitter/user/` | `lists`, `accounts` |
| `reddit` | Native Reddit RSS (`/r/<sub>/<sort>.rss`) | `subreddit`, `sort` |
| `rsshub` | Any RSSHub route | `route` (e.g. `/hackernews/best`, `/arstechnica/index`) |
| `rss` | Direct RSS/Atom feed | `url` |

All feed types support `count` (max items) and `name` (identifier for dedup tracking). See `skills/scan_feeds/config.toml` for examples.

---

## Delegation

Skills are included in the `delegate_task` and `plan_and_execute` tool's allowed tool set. When skills are created or updated, `rebuildGrammar()` adds them to the delegate config's tool list and grammar.
