# Sokratos

An autonomous AI assistant with long-term memory, powered by a multi-model architecture running on local llama-server instances. Interacts via Telegram, manages email and calendar through Gmail/Google Calendar OAuth, and maintains a persistent memory system backed by PostgreSQL with pgvector.

## Architecture

Sokratos uses a **single-model supervisor pattern** where a vision-capable orchestrator produces free-form text with tool intent tags parsed via regex. Five llama-server instances run on a separate Mac (M3 Ultra):

| Service | Port | Model | Purpose |
|---------|------|-------|---------|
| Orchestrator | 11434 | Qwen3.5-VL-35B-A3B (UD-Q4_K_XL) | Vision-capable MoE reasoning, free-form text + `<TOOL_INTENT>` tags |
| Deep thinker | 11435 | GLM-Z1-32B (UD-Q6_K_XL) | Always-on heavy reasoning, consolidation |
| Subagent | 11436 | GLM-4.7-Flash-30B-A3B (UD-Q4_K_XL) | Always-on MoE structured tasks: triage, rewriting, re-ranking |
| On-demand router | 11437 | Arctic-Text2SQL-7B (Q8_0) | SQL generation (loaded/unloaded dynamically) |
| Embedding | 8081 | BGE-large-en-v1.5 (q8_0) | 1024-dim vectors for pgvector |

The orchestrator runs without grammar constraints. When it wants a tool, it emits `<TOOL_INTENT>tool: {params}</TOOL_INTENT>` tags. `parseToolIntent()` extracts the tool name and JSON arguments using regex. The tool executes, the result is injected back, and the loop repeats (max 15 rounds).

### Model Lineage Diversity

GLM-Z1-32B is deliberately chosen for the deep thinker over a Qwen-family model to avoid compounded inferential mistakes when the Qwen3-VL orchestrator gets a "second opinion" on triage, consolidation, or reasoning tasks.

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

This launches all five llama-server instances.

### 3. Configure environment

Copy `.env.example` to `.env` (or create `.env`) with at minimum:

```bash
TELEGRAM_BOT_TOKEN=your_token
ALLOWED_TELEGRAM_IDS=your_telegram_id
DATABASE_URL=postgres://sokratos:sokratos@localhost:5435/sokratos

LLM_URL=http://your-mac:11434
LLM_MODEL=Qwen3-VL-30B-A3B-Instruct-UD-Q6_K_XL

DEEP_THINKER_URL=http://your-mac:11435
DEEP_THINKER_MODEL=GLM-Z1-32B-0414-UD-Q6_K_XL

TEXT2SQL_URL=http://your-mac:11437

SUBAGENT_URL=http://your-mac:11436
SUBAGENT_MODEL=GLM-4.7-9B-Flash-UD-Q4_K_XL

EMBEDDING_URL=http://your-mac:8081
EMBEDDING_MODEL=bge-large-en-v1.5-q8_0

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
| `LLM_URL` | `http://localhost:11434` | Orchestrator endpoint |
| `LLM_MODEL` | (required) | Orchestrator model name |
| `DEEP_THINKER_URL` | (empty) | Deep thinker endpoint |
| `DEEP_THINKER_MODEL` | (empty) | Deep thinker model (no .gguf suffix) |
| `TEXT2SQL_URL` | (empty) | Text2SQL on-demand router endpoint |
| `TEXT2SQL_MODEL` | `Arctic-Text2SQL-R1-7B.Q8_0` | Text2SQL model name |
| `TEXT2SQL_KEEP_ALIVE` | `30s` | Text2SQL auto-unload delay |
| `SUBAGENT_URL` | (empty) | Subagent endpoint |
| `SUBAGENT_MODEL` | (empty) | Subagent model name |
| `SUBAGENT_SLOTS` | `2` | Concurrent slots for Flash subagent |
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
| `MAX_TOOL_RESULT_LEN` | `2000` | Truncate tool results beyond this |
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
| `bootstrap_profile` | Generate initial identity profile from conversation context |
| `consult_deep_thinker` | Route complex reasoning to GLM-Z1-32B with full chain-of-thought |
| `delegate_task` | Delegate structured tasks to subagent with scoped tool access |
| `plan_and_execute` | Decompose a directive into steps via DTC, execute via subagent (supports background mode) |
| `check_background_task` | List, check status, or cancel background tasks |
| `ask_database` | Natural language queries against the PostgreSQL database via Text2SQL |
| `search_email` | Search Gmail with time bounds and query filters |
| `send_email` | Send an email (requires Telegram confirmation) |
| `search_calendar` | Search Google Calendar events with time bounds |
| `create_event` | Create a calendar event (requires Telegram confirmation) |
| `search_web` | Web search via SearXNG |
| `read_url` | Fetch and extract content from a web page |
| `run_code` | Execute JavaScript code in a sandboxed goja runtime |
| `add_task` / `complete_task` | Manage scheduled tasks with recurrence |
| `manage_routines` | Create persistent background habits (e.g., "check email every 2h") |
| `manage_personality` | Set, remove, or list personality traits |
| `update_state` | Update the agent's current status and task |
| `set_preference` | Store quick-access user preferences |
| `create_skill` / `manage_skills` | Create or manage user-defined JavaScript tools |

## Memory System

See [docs/memory-system.md](docs/memory-system.md) for the full technical reference.

In brief: memories are stored as 1024-dim vectors in PostgreSQL with pgvector (HNSW index). A composite ranking formula combines cosine similarity, BM25 full-text search, salience, usefulness feedback, confidence, retrieval popularity, entity matching, and temporal recency. Memories decay with a ~30-day half-life and are pruned when stale. Higher-order synthesis layers (consolidation, episodic synthesis, reflection) are triggered by event-driven cognitive processing based on memory volume and user activity lulls.

## Project Structure

```
sokratos/
  main.go              # Telegram bot, message loop, prefetch, wiring
  config.go            # Environment variable helpers
  confirm.go           # Telegram confirmation gate for sensitive tools
  prefetch.go          # Subconscious memory prefetch for message loop
  telegram.go          # Telegram helpers (photo download, typing indicator)
  format.go            # Markdown-to-Telegram HTML converter
  db/                  # PostgreSQL connection and schema auto-apply
    schema.sql         # memories, tasks, routines, personality_traits, etc.
  engine/              # Heartbeat loop, context sliding, state
    engine.go          # Heartbeat: phase 1 routines, phase 2 contextual reasoning
    cognitive.go       # Event-driven cognitive processing (consolidation, episodes, reflection)
    routines.go        # Persistent background routine execution
    scheduler.go       # PostgreSQL-backed task scheduler
    slide.go           # Context window management and archival (ArchiveDeps)
    state.go           # Thread-safe in-memory agent state with DB-backed prefs
  llm/                 # LLM client, supervisor pattern, tool intent extraction
    client.go          # Chat API, thinking mode, grammar constraints
    supervisor.go      # Supervisor loop with regex-based tool parsing
  memory/              # Embedding, storage, scoring, decay, synthesis
    save.go            # SaveToMemoryAsync, SaveToMemoryWithSalienceAsync, identity profile
    embedding.go       # Embed + chunk helpers
    scoring.go         # Quality scoring (entities, confidence, contradiction detection)
    ranking.go         # Shared SQL ranking formula (RankingOrderBy)
    bm25.go            # Client-side BM25 utilities
    decay.go           # Salience decay and usefulness regression
    episodes.go        # Episodic memory synthesis
    reflection.go      # Meta-cognitive reflection and retrieval tracking
    personality.go     # Personality traits (DB read/write, prompt formatting)
    prefetch.go        # Shared prefetch logic for message loop and heartbeat
    format.go          # Memory formatting utilities
  tools/               # Tool implementations and registry
    tools.go           # Registry, Execute, NewScopedToolExec factory
    memory.go          # search_memory, save_memory, retrieval tracking
    triage_conversation.go  # TriageConfig, TriageSaveRequest, triageAndSave core pipeline
    triage_queue.go    # Retry queue wrappers for conversation/email triage
    consolidate_memory.go   # Profile synthesis pipeline
    deep_thinker_client.go  # Shared HTTP client with think/no-think modes
    subagent_client.go # Concurrency-limited subagent client
    subagent_supervisor.go  # Multi-turn subagent supervisor loop
    base_client.go     # Shared HTTP base for DTC and subagent
    processed_tracker.go    # Dedup abstraction for processed_emails/processed_events
    skills.go          # Skill loader, executor, and registry integration
    create_skill.go    # create_skill tool (JS validation, test, persist)
    personality.go     # manage_personality tool
    transition.go      # Paradigm shift transition memory generation
    routines.go        # manage_routines tool
    delegate_task.go   # delegate_task tool (scoped tool access)
    plan_execute.go    # plan_and_execute + check_background_task tools
    search_web.go      # search_web tool (SearXNG)
    read_url.go        # read_url tool (web page fetching)
    run_code.go        # run_code tool (sandboxed JS execution)
    retry_queue.go     # Background retry queue for triage failures
  httputil/            # HTTP client factory (shared transport config)
  textutil/            # Shared text processing (strip tags, extract JSON, truncate)
  googleauth/          # OAuth2 helpers for Gmail/Calendar via Telegram
  grammar/             # GBNF grammar builder from tool schemas
  prompts/             # Embedded prompt templates (//go:embed)
  skills/              # User-created JavaScript tools (auto-loaded on startup)
  timeouts/            # Shared timeout constants (DB, embedding, synthesis, save)
  models/              # GGUF model files and start.sh
  docker/              # Docker Compose for PostgreSQL + SearXNG
```

## Development

```bash
go build ./...   # build (prompts are embedded via //go:embed)
go vet ./...     # lint
go test ./...    # run tests
```

Prompts in `prompts/` are embedded at compile time. Any prompt change requires a rebuild.

## License

Private repository.
