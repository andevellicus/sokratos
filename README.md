# Sokratos

An autonomous AI assistant with long-term memory, powered by a multi-model architecture running on local llama-server instances. Interacts via Telegram, manages email and calendar through Gmail/Google Calendar OAuth, and maintains a persistent memory system backed by PostgreSQL with pgvector.

## Architecture

Sokratos uses a **three-tier dispatch architecture** with two LLM backends and a **supervisor pattern with regex-based tool parsing**. Three llama-server instances run on a separate Mac (M3 Ultra):

| Service | Port | Model | Purpose |
|---------|------|-------|---------|
| Brain | 11434 | Qwen3.5-122B-A10B (UD-Q4_K_XL) | Deep reasoning, orchestrator for complex requests, consolidation |
| 9B | 11436 | Qwen3.5-9B (UD-Q4_K_XL) | Dispatch triage, synthesis, subagent tasks, orchestrator fallback (3 slots) |
| Embedding | 8081 | Qwen3-Embedding-4B (Q8_0) | 2560-dim vectors for pgvector |

### Message Flow

Every user message goes through the 9B for **grammar-constrained triage**, which decides one of three paths:

1. **Dispatch (single tool)** — 9B sends an ack, executes the tool, and synthesizes the response. Fully handled by the 9B.
2. **Dispatch (multi-step)** — 9B sends an ack, then a `SubagentSupervisor` loop runs 2-5 tool rounds with grammar-constrained decisions.
3. **Escalate to orchestrator** — the 9B orchestrator handles the request directly (including tool calls). For complex reasoning, the orchestrator can invoke the `reason` tool to escalate to the Brain (122B). A "Thinking..." notification is sent to the user during Brain calls.

A **slot router** manages backend allocation — Brain preferred for interactive messages (accuracy), 9B preferred for background work (throughput). During tool execution, slots are released and reacquired after, preventing long-running tools from blocking LLM access. See [docs/dispatch.md](docs/dispatch.md) for the full technical reference.

### Supervisor Pattern

The orchestrator runs without grammar constraints, emitting `<TOOL_INTENT>tool: {params}</TOOL_INTENT>` tags. `parseToolIntent()` extracts the tool name and JSON arguments using regex. The tool executes, the result is injected back, and the loop repeats (max 15 rounds). The Brain's system prompt ensures intermediate prose is discarded — only the final message reaches the user.

### Client Hierarchy

Both LLM backends share a `baseClient` (`clients/base_client.go`) providing HTTP transport, health probes (`ensureLoaded`), and circuit breakers. Two specialized clients build on it:

- **DeepThinkerClient** — Brain (122B): `Complete()` with chain-of-thought thinking, `CompleteNoThink()` without
- **SubagentClient** — 9B: semaphore-gated structured tasks, grammar-constrained output, concurrent slot management

A **work queue** (`clients/workqueue.go`) manages background LLM tasks with priority-based admission control, retry with backoff, and deferred re-queuing of low-priority items under pressure.

## Prerequisites

- **Go 1.25+**
- **PostgreSQL 17** with pgvector extension (provided via Docker)
- **llama-server** (llama.cpp) on a machine with sufficient VRAM
- **SearXNG** (optional, for web search)
- **Gmail/Calendar OAuth credentials** (optional, for email/calendar features)

## Quick Start

### 1. Start infrastructure

```bash
cd docker && docker compose up -d
```

This starts PostgreSQL (port 5435) and SearXNG (port 9000).

### 2. Start inference servers

On your inference machine (e.g., Mac with M3 Ultra):

```bash
cd models && ./start.sh
```

This launches the llama-server instances.

### 3. Configure environment

Copy `.env.example` to `.env` (or create `.env`) with at minimum:

```bash
TELEGRAM_BOT_TOKEN=your_token
ALLOWED_TELEGRAM_IDS=your_telegram_id
DATABASE_URL=postgres://sokratos:sokratos@localhost:5435/sokratos

BRAIN_URL=http://your-mac:11434
BRAIN_MODEL=Qwen3.5-122B-A10B-UD-Q4_K_XL

SUBAGENT_URL=http://your-mac:11436
SUBAGENT_MODEL=Qwen3.5-9B-UD-Q4_K_XL
SUBAGENT_SLOTS=3

EMBEDDING_URL=http://your-mac:8081
EMBEDDING_MODEL=Qwen3-Embedding-4B-Q8_0

SEARXNG_URL=http://localhost:9000
```

### 4. Build and run

```bash
go build -o sokratos ./...
./sokratos
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TELEGRAM_BOT_TOKEN` | (required) | Telegram Bot API token |
| `ALLOWED_TELEGRAM_IDS` | (empty = allow all) | Comma-separated Telegram user IDs |
| `DATABASE_URL` | (empty) | PostgreSQL connection string |
| `BRAIN_URL` | (required) | Brain (122B) endpoint |
| `BRAIN_MODEL` | (required) | Brain model name (no .gguf suffix) |
| `SUBAGENT_URL` | (required) | 9B subagent/orchestrator endpoint |
| `SUBAGENT_MODEL` | (required) | 9B model name |
| `SUBAGENT_SLOTS` | `3` | Concurrent 9B slots (1 supervisor + 2 subagent) |
| `EMBEDDING_URL` | (empty) | Embedding endpoint |
| `EMBEDDING_MODEL` | (empty) | Embedding model name |
| `SEARXNG_URL` | (empty) | SearXNG instance URL |
| `HEARTBEAT_INTERVAL` | `5m` | Autonomous heartbeat tick |
| `MAINTENANCE_INTERVAL` | `30m` | Interval between maintenance runs (decay, pruning) |
| `COGNITIVE_BUFFER_THRESHOLD` | `20` | Min unreflected memories to trigger cognitive processing |
| `LULL_DURATION` | `20m` | Min user idle time before cognitive processing |
| `COGNITIVE_CEILING` | `4h` | Max time between cognitive runs |
| `REFLECTION_MEMORY_THRESHOLD` | `50` | Trigger reflection after this many new memories |
| `MEMORY_SEARCH_LIMIT` | `10` | Max results from search_memory |
| `MEMORY_STALENESS_DAYS` | `90` | Prune decayed memories older than this |
| `CONSOLIDATION_MEMORY_LIMIT` | `50` | Max memories per consolidation pass |
| `MAX_TOOL_RESULT_LEN` | `8000` | Truncate tool results beyond this |
| `MAX_WEB_SOURCES` | `2` | Max web pages to read per query |
| `DB_MAX_CONNS` | `20` | Max database pool connections |
| `DB_MIN_CONNS` | `2` | Min idle database connections |
| `DB_MAX_CONN_LIFETIME` | `30m` | Max lifetime per database connection |
| `DB_MAX_CONN_IDLE_TIME` | `5m` | Max idle time before connection close |
| `DB_HEALTH_CHECK_PERIOD` | `30s` | Database health check interval |
| `CONFIRMATION_TIMEOUT` | `2m` | Timeout for Telegram confirmation prompts |
| `EMAIL_CHECK_LOOKBACK` | `newer_than:1h` | Gmail query fragment for new-email check |
| `EMAIL_DISPLAY_BATCH` | `5` | Max emails shown to orchestrator per check |
| `GMAIL_CREDENTIALS_PATH` | `.credentials/credentials.json` | OAuth credentials file |
| `GMAIL_TOKEN_PATH` | `.credentials/token.json` | Gmail OAuth token |
| `CALENDAR_TOKEN_PATH` | `.credentials/calendar_token.json` | Calendar OAuth token |

### Advanced Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_NAME` | `Sokratos` | Display name used in system prompt and logs |
| `BOOTSTRAP_PROMPT_PATH` | (empty) | Override bootstrap prompt from file instead of embedded default |
| `BOOTSTRAP_CONTEXT_PATH` | (empty) | Load bootstrap context from file |
| `BOOTSTRAP_CONTEXT` | (empty) | Inline bootstrap context string (alternative to file) |

## Tools

The orchestrator has access to the following built-in tools:

| Tool | Description |
|------|-------------|
| `search_memory` | Semantic search over long-term memory with query rewriting, multi-embedding retrieval, entity graph hops, and re-ranking |
| `save_memory` | Persist a fact or preference to long-term memory |
| `forget_topic` | Delete memories related to a topic by semantic similarity |
| `consolidate_memory` | Synthesize high-salience memories into the core profile |
| `reason` | Route complex reasoning to the Brain model with full chain-of-thought. Supports `background=true` to spawn a background Brain session with tool access for complex tasks (skill creation, research, analysis). |
| `delegate_task` | Delegate structured tasks to subagent with scoped tool access |
| `plan_and_execute` | Decompose a directive into steps via DTC, execute via subagent (supports background mode) |
| `check_background_task` | List, check status, or cancel background tasks |
| `ask_database` | Natural language queries against the PostgreSQL database via subagent |
| `search_email` | Search Gmail with time bounds and query filters |
| `send_email` | Send an email (requires Telegram confirmation) |
| `search_calendar` | Search Google Calendar events with time bounds |
| `create_event` | Create a calendar event (requires Telegram confirmation) |
| `search_web` | Web search via SearXNG |
| `read_url` | Fetch and extract content from a web page |
| `run_code` | Execute JavaScript code in a sandboxed goja runtime |
| `add_task` / `complete_task` | Manage scheduled tasks with recurrence |
| `manage_routines` | Create persistent background habits with action args and templates — syncs to `.config/routines.toml` |
| `manage_personality` | Set, remove, or list personality traits |
| `update_state` | Update the agent's current status and task |
| `set_preference` | Store quick-access user preferences |
| `create_skill` / `update_skill` / `manage_skills` | Create, update, or manage user-defined TypeScript/JavaScript tools |
| `manage_objectives` | Add, update, pause, resume, complete, retire, or list long-term objectives |
| `reply_to_job` / `cancel_job` | Interact with or cancel background Brain jobs |
| `run_command` | Execute shell commands in a sandboxed environment |
| `read_file` / `write_file` / `patch_file` / `list_files` | File system operations within the workspace directory |

## Skill System

Skills are user-created TypeScript tools that persist to disk and auto-load on startup. Each skill lives in `skills/<name>/` with a `SKILL.md` manifest, `scripts/handler.ts`, and optional `config.toml`. TypeScript is transpiled to JavaScript via esbuild at load time (pure Go, <1ms). Plain `.js` handlers are also supported as a fallback.

### Runtime Globals

Skills execute in a goja ES2020 sandbox (30s timeout; 5min with delegation deps) with:

| Global | Description |
|--------|-------------|
| `args` | Parsed JSON parameters from the tool invocation |
| `skill_config` | Parsed `config.toml` as a JS object |
| `http_request(method, url, headers, body)` | Synchronous HTTP bridge (15s timeout, 1MB cap, private IPs blocked) |
| `console.log/warn/error(...)` | Output appended to result as log lines |
| `btoa(s)` / `atob(s)` | Base64 encode/decode |
| `sleep(ms)` | Synchronous sleep (capped at 5s per call) |
| `env(key)` | Read `SKILL_<key>` environment variable |
| `kv_get(key)` / `kv_set(key, value)` / `kv_delete(key)` | Per-skill PostgreSQL key-value store |
| `hash_sha256(s)` | SHA-256 hex digest |
| `hash_hmac_sha256(key, msg)` | HMAC-SHA256 hex digest |
| `call_tool(name, args)` | Synchronous tool invocation (self-call prevented) |
| `delegate(directive, context)` | Single subagent dispatch (60s timeout) |
| `delegate_batch(tasks)` | Parallel fan-out (`[{directive, context}, ...]` → `[{result, error}, ...]`, 3min timeout) |

### Built-in Skills

| Skill | Config | Output |
|-------|--------|--------|
| `get_weather` | `location` | `{location, current: {condition, temp_f, humidity, ...}, forecast: [{date, high_f, low_f, condition}]}` |
| `scan_feeds` | `feeds` | RSS/Atom aggregator with parallel article summarization. Supports Twitter (via RSSHub), Reddit (native RSS), direct RSS/Atom, and any RSSHub route. |
| `weekly_review` | — | Generates a structured weekly review by querying memory activity, active goals, work items, routine health, and personality evolution. |

### Creating Skills

The orchestrator can create new skills at runtime via `create_skill`. Skills are validated (transpiled if TypeScript, test execution), written to disk, registered in the live tool registry, and the GBNF grammar is rebuilt. Skills can also be managed via `manage_skills` (list/delete).

## Dispatch Architecture

See [docs/dispatch.md](docs/dispatch.md) for the full technical reference on message routing, triage, slot routing, and synthesis.

## Background Brain Jobs

Complex tasks (skill creation, research, multi-step analysis) can be offloaded to background Brain sessions that run concurrently while the 9B continues serving the user. Two paths converge:

1. **Mandatory intercept** — `create_skill` and `update_skill` are intercepted at the supervisor level and always routed to a background Brain session.
2. **Voluntary** — The 9B calls `reason(background=true, task_type="...")` to spawn a background Brain session for any complex task.

Background jobs support multi-round tool execution and can ask the user clarifying questions (the job parks until input arrives via `reply_to_job`). Jobs can be cancelled via `cancel_job`. Session prompts are selected by `task_type` (e.g. `"create_skill"` uses a skill-creation prompt, general tasks use a reasoning prompt).

## Memory System

See [docs/memory-system.md](docs/memory-system.md) for the full technical reference.

In brief: memories are stored as 2560-dim vectors (Qwen3-Embedding-4B) in PostgreSQL with pgvector. At current scale, brute-force scan is used (HNSW/IVFFlat cap at 2000 dims). A composite ranking formula combines cosine similarity, BM25 full-text search, salience, usefulness feedback, confidence, retrieval popularity, entity matching, and temporal recency. Memories decay with a dual-rate system: unretrieved memories (age>14d) use a ~15-day half-life, all others use ~30-day. Episode synthesis clusters related memories and reduces constituent salience by 40% so episodes are preferred in retrieval. Triage includes context-aware coverage checks — if 3+ similar memories already exist, the save bar is raised. Paradigm shifts trigger a fast-path: synchronous transition memory → mini-consolidation → immediate profile refresh. Higher-order synthesis layers (consolidation, episodic synthesis, reflection) are triggered by event-driven cognitive processing based on memory volume and user activity lulls. Reflection insights are routed back into conversation context for the orchestrator.

## Routines

Routines are persistent background habits defined in `.config/routines.toml` and synced to PostgreSQL. They execute during the heartbeat loop when their trigger fires. All routine logic lives in the `routines/` package.

### Structured Format (preferred)

```toml
[feed-digest]
interval = "4 hours"
action = "scan_feeds"
goal = "Select 3-5 most interesting items. Send a digest with title, summary, and link."
silent_if_empty = true
```

When `action` is set, the engine calls it directly, then passes the result + `goal` to the orchestrator for interpretation. If `silent_if_empty = true` and the action returns no data, the orchestrator is skipped entirely (no message sent).

### Multi-Action with Action Args

```toml
[morning-briefing]
schedule = "06:00"
actions = ["search_email", "get_weather", "search_calendar"]
goal = "Synthesize everything into a concise daily orientation."

[morning-briefing.action_args.search_calendar]
time_min = "{{today}}"
time_max = "{{tomorrow}}"
```

`action_args` provides per-action arguments with template expansion at execution time. Non-string values (numbers, booleans) pass through as-is — you can pass any argument a tool or skill accepts.

| Template | Expands to | Example |
|----------|-----------|---------|
| `{{today}}` | Today 00:00 | `2026-03-02T00:00:00` |
| `{{tomorrow}}` | Tomorrow 00:00 | `2026-03-03T00:00:00` |
| `{{yesterday}}` | Yesterday 00:00 | `2026-03-01T00:00:00` |
| `{{now}}` | Current time | `2026-03-02T14:30:00` |
| `{{base+offset}}` | Base time + offset | `{{now-2h}}`, `{{today+3d}}` |

Offset units: `m` (minutes), `h` (hours), `d` (days), `w` (weeks). All times in local timezone.

### Triggers

Routines support three trigger modes:

- **Interval**: `interval = "4 hours"` — fires when the duration elapses since last execution
- **Schedule**: `schedule = "06:00"` or `schedule = ["06:00", "18:00"]` — fires at specific daily times (multi-time supported)
- **Combined**: Both `interval` and `schedule` on the same routine — fires on whichever trigger comes first

### Legacy Format

```toml
[check-inbox]
interval = "2 hours"
instruction = "Check for new emails and alert about urgent ones."
```

Without an `action` field, the full instruction is passed to the orchestrator which handles everything.

### Source of Truth

`.config/routines.toml` is the source of truth. The database is a runtime cache. Three sync paths keep them aligned:

1. **Startup** — `routines.SyncFromFile()` does a full sync (upsert all TOML entries, delete DB entries not in the file)
2. **Heartbeat** — mtime-based incremental check on each tick; re-syncs if the file was modified
3. **`/reload`** — Telegram command that forces an immediate full sync of both routines and skills

Changes made via the `manage_routines` tool are written back to `.config/routines.toml`.

See `.config/routines.toml.example` for more examples.

## Project Structure

```
sokratos/
  main.go              # Entry point, serviceBundle, initServices, initLLM, message loop
  wire.go              # initEngine, wireEngine (post-construction wiring), startup tasks
  adapters.go          # Engine interface adapters (notifier, hot-reload, cognitive, reflection)
  message_loop.go      # processMessage, completeMessageHandling, command handlers
  dispatch.go          # Dispatch triage, multi-step dispatch, background Brain jobs, prompt builders
  condense.go          # Tool result condensation and context summarization
  prefetch.go          # Subconscious memory prefetch and usefulness evaluation
  objective_callbacks.go # Objective task completion and share gate wiring
  register_tools.go    # Domain-grouped tool registration
  confirm.go           # Telegram confirmation gate for sensitive tools
  telegram.go          # Telegram helpers (photo download, typing indicator)
  format.go            # Markdown-to-Telegram HTML converter
  adaptive/            # Runtime-tunable thresholds (Get/Set/Clamp, adaptive_params table)
  clients/             # LLM client hierarchy (baseClient, DTC, SubagentClient)
    base_client.go     # Shared HTTP client, health probe, circuit breaker
    deep_thinker_client.go  # Brain (122B) — thinking/no-think modes
    subagent_client.go      # 9B — semaphore-gated structured tasks
    subagent_supervisor.go  # Multi-turn grammar-constrained tool loop
    workqueue.go       # Priority work queue with retry and admission control
  config/              # AppConfig struct and env-var parsing
  db/                  # PostgreSQL connection and schema auto-apply
    schema.sql         # memories, objectives, routines, adaptive_params, etc.
  engine/              # Heartbeat loop, context sliding, state, slot routing
    engine.go          # Engine struct and Run() (3 independent loops)
    interfaces.go      # Notifier, HotReloader, CognitiveServices, ReflectionSink
    background_jobs.go # BackgroundJob struct and StateManager job management
    cognitive.go       # Event-driven cognitive processing (consolidation, episodes, reflection)
    curiosity.go       # Signal-driven curiosity dispatch
    heartbeat.go       # Heartbeat loop tick (Phase 1: gatekeeper triage)
    heartbeat_phase2.go # Heartbeat Phase 2 (staleness detection, orchestrator call)
    objectives.go      # Objective inference from conversation context
    objective_pursuit.go # Cooldown-gated objective pursuit
    routines.go        # Routine execution within heartbeat (uses routines/ package)
    scheduler.go       # PostgreSQL-backed task scheduler
    share_limiter.go   # Rate-limited proactive sharing (max N/day, 30min gap)
    slide.go           # Context window management and archival
    slot_router.go     # Routes orchestrator calls between Brain and 9B
    state.go           # Thread-safe in-memory agent state with DB-backed prefs
    temporal.go        # Temporal context builder (upcoming tasks, events)
    trim.go            # Tool result trimming for orchestrator context
  objectives/          # Objective CRUD (Create, Get, ListActive, Complete, Retire, etc.)
  routines/            # Routine definitions, scheduling, file I/O, DB sync
    routines.go        # Entry, DueRoutine, NilIfEmpty, IsEmptyResult
    schedule.go        # NormalizeSchedule, ParseSchedules, IsScheduleDue (multi-time)
    args.go            # ExpandArgs, ExpandAndMarshal (template expansion)
    file.go            # FileWriter interface, FileAdapter, LoadFile
    sync.go            # SyncFromFile, SyncIfChanged, Upsert, Delete, QueryDue, AdvanceTimer
  llm/                 # LLM client, supervisor pattern, tool intent extraction
    client.go          # Chat API, QueryOrchestrator, BackgroundJobRequest, FallbackMap
    supervisor.go      # Supervisor loop with regex-based tool parsing
  memory/              # Embedding, storage, scoring, decay, synthesis
    save.go            # SaveToMemoryAsync, SaveToMemoryWithSalienceAsync, identity profile
    embedding.go       # Embed + chunk helpers
    scoring.go         # Quality scoring (entities, confidence, contradiction detection)
    ranking.go         # Shared SQL ranking formula (RankingOrderBy)
    bm25.go            # Client-side BM25 utilities
    decay.go           # Salience decay and usefulness regression
    episodes.go        # Episodic memory synthesis (thematic clustering)
    reflection.go      # Meta-cognitive reflection, retrieval tracking, adaptive params
    personality.go     # Personality traits (DB read/write, prompt formatting)
    prefetch.go        # Shared prefetch logic for message loop and heartbeat
    failed_ops.go      # Failed operation logging and recent failure queries
    format.go          # Memory formatting utilities
  tools/               # Tool implementations and registry
    tools.go           # Registry, Execute, NewScopedToolExec factory
    timeouts.go        # Centralized tool-level timeouts (dispatch, plan, skill, etc.)
    memory.go          # search_memory, save_memory, retrieval tracking
    skills.go          # Skill loader, executor, and registry integration
    create_skill.go    # create_skill tool (TS transpile, test, persist)
    transpile.go       # TypeScript → JS transpilation via esbuild
    skill_vm.go        # Goja VM setup and skill runtime globals
    deep_thinker.go    # reason tool (deep thinking + background Brain sessions)
    personality.go     # manage_personality tool
    objectives.go      # manage_objectives tool
    routines.go        # manage_routines tool (uses routines/ package)
    delegate_task.go   # delegate_task tool (scoped tool access)
    plan_execute.go    # plan_and_execute + check_background_task tools
    background_jobs.go # reply_to_job + cancel_job tools
    shell.go           # run_command tool (sandboxed shell execution)
    search_web.go      # search_web tool (SearXNG)
    read_url.go        # read_url tool (web page fetching)
    run_code.go        # run_code tool (sandboxed JS execution)
    work_tracker.go    # WorkTracker for background plans, routines, scheduled tasks
  pipelines/           # Multi-step processing pipelines
    consolidate_memory.go  # Memory consolidation (ConsolidateCore, ConsolidateImmediate)
    triage_conversation.go # Conversation/email triage and save
    transition.go      # Paradigm shift fast-path
    bootstrap_profile.go   # Initial profile generation (/bootstrap)
    triage_queue.go    # Deferred triage retry queue
  google/              # Google OAuth2, Gmail, Calendar clients
  httputil/            # HTTP client factory (shared transport config)
  textutil/            # Shared text processing (strip tags, extract JSON, truncate)
  grammar/             # GBNF grammar builders (triage, dispatch, tool schemas)
  prompts/             # Embedded prompt templates (//go:embed)
  skills/              # JavaScript/TypeScript tools (auto-loaded on startup)
    <name>/SKILL.md    # Frontmatter manifest (name, description, parameters)
    <name>/scripts/handler.ts  # Skill source code (TS preferred, JS fallback)
    <name>/config.toml # Optional TOML config (injected as skill_config)
  .config/routines.toml  # Routine definitions (synced bidirectionally with DB)
  timeouts/            # Shared timeout constants (DB, embedding, synthesis, save)
  timefmt/             # Centralized time formatting constants and helpers
  models/              # GGUF model files and start.sh
  docker/              # Docker Compose for PostgreSQL + SearXNG
```

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/bootstrap` | Generate initial identity profile from conversation context |
| `/reload` | Force re-sync routines and skills from TOML files to database |
| `/google` | Re-authenticate Google OAuth (Gmail + Calendar) via Telegram |

## Development

```bash
go build ./...   # build (prompts are embedded via //go:embed)
go vet ./...     # lint
go test ./...    # run tests
```

Prompts in `prompts/` are embedded at compile time. Any prompt change requires a rebuild.

## License

Private repository.
