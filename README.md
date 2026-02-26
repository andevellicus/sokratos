# Sokratos

An autonomous AI assistant with long-term memory, powered by a multi-model architecture running on local llama-server instances. Interacts via Telegram, manages email and calendar through Gmail/Google Calendar OAuth, and maintains a persistent memory system backed by PostgreSQL with pgvector.

## Architecture

Sokratos uses a **two-model supervisor pattern** where a vision-capable orchestrator produces free-form text with tool intent tags, and a dedicated tool agent translates those intents into structured JSON constrained by GBNF grammar. Four llama-server instances run on a separate Mac (M3 Ultra):

| Service | Port | Model | Purpose |
|---------|------|-------|---------|
| Orchestrator | 11434 | Qwen3-VL-30B-A3B | Vision-capable reasoning, free-form text |
| On-demand router | 11435 | GLM-Z1-32B + Arctic-Text2SQL-7B | Deep thinking & SQL generation (loaded/unloaded dynamically) |
| Tool agent | 11436 | Granite 3.3 8B | Structured JSON tool calls via GBNF grammar |
| Embedding | 8081 | BGE-large-en-v1.5 | 1024-dim vectors for pgvector |

The orchestrator runs without grammar constraints. When it wants a tool, it emits `<TOOL_INTENT>tool: {params}</TOOL_INTENT>` tags. The tool agent receives the intent and produces structured JSON. The tool executes, the result is injected back, and the loop repeats (max 15 rounds).

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

This launches all four llama-server instances.

### 3. Configure environment

Copy `.env.example` to `.env` (or create `.env`) with at minimum:

```bash
TELEGRAM_BOT_TOKEN=your_token
ALLOWED_TELEGRAM_IDS=your_telegram_id
DATABASE_URL=postgres://sokratos:sokratos@localhost:5435/sokratos

LLM_URL=http://your-mac:11434
LLM_MODEL=Qwen3-VL-30B-A3B-Instruct-UD-Q6_K_XL

DEEP_THINKER_URL=http://your-mac:11435
DEEP_THINKER_MODEL=GLM-Z1-32B-0414-Q4_K_M

TEXT2SQL_URL=http://your-mac:11435

TOOL_AGENT_URL=http://your-mac:11436
TOOL_AGENT_MODEL=granite-3.3-8b-instruct-Q8_0

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
| `TEXT2SQL_URL` | (empty) | Text2SQL endpoint |
| `TOOL_AGENT_URL` | (empty) | Tool agent endpoint |
| `TOOL_AGENT_MODEL` | (empty) | Tool agent model name |
| `EMBEDDING_URL` | (empty) | Embedding endpoint |
| `EMBEDDING_MODEL` | (empty) | Embedding model name |
| `SEARXNG_URL` | (empty) | SearXNG instance URL |
| `HEARTBEAT_INTERVAL` | `5m` | Autonomous heartbeat tick |
| `CONSOLIDATION_INTERVAL` | `1h` | Memory consolidation tick |
| `EPISODE_SYNTHESIS_INTERVAL` | `6h` | Episodic memory synthesis tick |
| `REFLECTION_MEMORY_THRESHOLD` | `50` | Trigger reflection after this many new memories |
| `MEMORY_SEARCH_LIMIT` | `10` | Max results from search_memory |
| `MEMORY_STALENESS_DAYS` | `90` | Prune decayed memories older than this |
| `CONSOLIDATION_MEMORY_LIMIT` | `50` | Max memories per consolidation pass |
| `MAX_TOOL_RESULT_LEN` | `2000` | Truncate tool results beyond this |
| `MAX_WEB_SOURCES` | `2` | Max web pages to read per query |
| `VRAM_PRESSURE_THRESHOLD` | `15.0` | Evict models when available memory % drops below this |
| `GMAIL_CREDENTIALS_PATH` | `.credentials/credentials.json` | OAuth credentials file |
| `GMAIL_TOKEN_PATH` | `.credentials/token.json` | Gmail OAuth token |
| `CALENDAR_TOKEN_PATH` | `.credentials/calendar_token.json` | Calendar OAuth token |

## Tools

The orchestrator has access to the following tools:

| Tool | Description |
|------|-------------|
| `search_memory` | Semantic search over long-term memory with query rewriting, multi-embedding retrieval, entity graph hops, and re-ranking |
| `save_memory` | Persist a fact or preference to long-term memory |
| `search_web` | Web search via SearXNG |
| `read_url` | Fetch and summarize a web page |
| `consult_deep_thinker` | Route complex reasoning to GLM-Z1-32B with full chain-of-thought |
| `ask_database` | Natural language queries against the PostgreSQL database via Text2SQL |
| `check_email` | Fetch, triage, and memorize new emails |
| `send_email` | Send an email (requires Telegram confirmation) |
| `search_inbox` | Search memorized emails by vector similarity |
| `check_calendar` | Fetch, triage, and memorize upcoming events |
| `create_event` | Create a calendar event (requires Telegram confirmation) |
| `search_calendar` | Search memorized calendar events |
| `consolidate_memory` | Synthesize high-salience memories into the core profile |
| `add_task` / `complete_task` | Manage scheduled tasks with recurrence |
| `manage_directives` | Create persistent background habits (e.g., "check email every 2h") |
| `update_state` | Update the agent's current status and task |
| `set_preference` | Store quick-access user preferences |
| `get_server_time` | Get the current server time |

## Memory System

See [docs/memory-system.md](docs/memory-system.md) for the full technical reference.

In brief: memories are stored as 1024-dim vectors in PostgreSQL with pgvector (HNSW index). A composite ranking formula combines cosine similarity, BM25 full-text search, salience, usefulness feedback, confidence, retrieval popularity, entity matching, and temporal recency. Memories decay with a ~30-day half-life and are pruned when stale. Higher-order synthesis layers create episodic memories (6h) and meta-cognitive reflections (7d).

## Project Structure

```
sokratos/
  main.go              # Telegram bot, message loop, prefetch, wiring
  config.go            # Environment variable helpers
  format.go            # Markdown-to-Telegram HTML converter
  db/                  # PostgreSQL connection and schema auto-apply
    schema.sql         # memories, tasks, directives, processed_* tables
  engine/              # Heartbeat loop, context sliding, VRAM auditor, state
    engine.go          # Tickers: heartbeat, consolidation, episodes, reflection
    slide.go           # Context window management and archival
    state.go           # Thread-safe disk-backed agent state
    vram_auditor.go    # Idle eviction and memory pressure detection
  llm/                 # LLM client, supervisor pattern, tool intent extraction
    client.go          # Chat API, thinking mode, grammar constraints
  memory/              # Embedding, storage, scoring, decay, synthesis
    memory.go          # Core: embed, chunk, save, score, contradict, synthesize
    ranking.go         # Shared SQL ranking formula (RankingOrderBy)
    bm25.go            # Client-side BM25 utilities
  tools/               # Tool implementations and registry
    memory.go          # search_memory, save_memory, entity graph, retrieval tracking
    deep_thinker_client.go  # Shared HTTP client with think/no-think modes
    consolidate_memory.go   # Profile synthesis pipeline
    triage_conversation.go  # Conversation triage and save
    check_items.go     # Shared email/calendar triage pipeline
    backfill.go        # Historical email/calendar import
  grammar/             # GBNF grammar builder from tool schemas
  prompts/             # Embedded prompt templates
    system.txt         # Main orchestrator system prompt
    tools.txt          # Tool descriptions for the orchestrator
    reflection.txt     # Meta-cognitive reflection prompt
    consolidation.txt  # Profile synthesis prompt
    *_triage.txt       # Email, calendar, conversation triage prompts
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
