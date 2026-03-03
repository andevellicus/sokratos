# Skill System

Skills are user-created JavaScript tools that persist to disk, auto-load on startup, and integrate with the tool registry and grammar system. They run in a sandboxed ES5 environment with HTTP bridging, key-value storage, and cryptographic helpers.

---

## Disk Layout

```
skills/
  <name>/
    SKILL.md              # Frontmatter manifest (name, description, parameters)
    scripts/handler.js    # Skill source code (ES5)
    config.toml           # Optional per-skill configuration (parsed as JS object)
```

### SKILL.md Format

```markdown
---
name: get-weather
description: |
  Fetch current weather and forecast for a configured location.
  Returns JSON with current conditions and 3-day forecast.
---

## Parameters

| Name | Type | Required |
|------|------|----------|
| location | string | no |
```

The YAML frontmatter defines `name`, `description`, and optionally documents parameters. The description is used in the tool's schema and in dynamic tool descriptions injected into the orchestrator's prompt.

### config.toml

Parsed via TOML into a JS object and injected as the `skill_config` global. Read fresh on each invocation (no restart needed). Falls back to `config.txt` as a raw string if TOML parsing fails.

```toml
location = "New York, NY"
units = "imperial"
```

Accessed in JS as `skill_config.location`, `skill_config.units`, etc.

---

## Runtime Environment

Skills execute in a **goja** ES5 sandbox with a 30-second timeout. The following globals are available:

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

### HTTP Bridge

`http_request(method, url, headers, body)` returns `{status, body, headers}`. Private/internal IPs are blocked by default unless the host appears in `AllowedInternalHosts` (populated from configured service URLs like SearXNG, RSSHub, etc.).

### Key-Value Store

The KV store is namespaced per skill in the `skill_kv` PostgreSQL table. Useful for dedup tracking (seen URLs), rate-limit backoff state, caching, etc. Values are strings — use `JSON.stringify`/`JSON.parse` for structured data.

---

## Lifecycle

### Creating Skills

Skills can be created two ways:

1. **`create_skill` tool** (runtime) — The orchestrator generates the JS code, validates syntax, runs a test execution with `test_args`, writes to disk, registers in the live registry, and rebuilds tool descriptions.

2. **Manual** — Create the directory structure by hand, then `/reload` or restart to pick it up.

### Hot-Reload

On each heartbeat tick, `SyncSkills()` checks mtimes of skill directories. Changed files trigger re-registration and grammar rebuild. Also triggered by `/reload`.

### Deletion

Via `manage_skills` tool (`action: "delete"`) or manual removal of the directory.

---

## Using Skills in Routines

Skills are tools like any other — they can be called from routines via the `tool` or `tools` field:

```toml
[feed-digest]
interval = "4 hours"
tool = "get-feeds"
goal = "Send a digest of the top items."
silent_if_empty = true
```

Tool arguments can be passed via `tool_args` with template expansion:

```toml
[feed-digest.tool_args.get-feeds]
count = 5
feed = "hackernews"
```

See [routines.md](routines.md) for the full template syntax.

---

## Built-in Skills

| Skill | Description | Key Config |
|-------|-------------|------------|
| `get-weather` | Current conditions + 3-day forecast via wttr.in | `location` |
| `get-feeds` | RSS/Atom aggregator: Twitter (RSSHub), Reddit (native RSS), any RSSHub route | `[[feeds]]` with `type`, `route`/`subreddit`/`lists` |

### get-feeds Feed Types

| Type | Source | Config Fields |
|------|--------|--------------|
| `twitter` | RSSHub `/twitter/list/` or `/twitter/user/` | `lists`, `accounts` |
| `reddit` | Native Reddit RSS (`/r/<sub>/<sort>.rss`) | `subreddit`, `sort` |
| `rsshub` | Any RSSHub route | `route` (e.g. `/hackernews/best`, `/arstechnica/index`) |

All feed types support `count` (max items) and `name` (identifier for dedup tracking). See `skills/get-feeds/config.toml.example` for examples.

---

## Delegation

Skills are included in the `delegate_task` and `plan_and_execute` tool's allowed tool set. When skills are created or updated, `rebuildGrammar()` adds them to the delegate config's tool list and grammar.
