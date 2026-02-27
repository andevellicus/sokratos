# Memory System Overview

The memory system is a **PostgreSQL + pgvector** store (1024-dim embeddings from BGE-large-en-v1.5) with multiple ingestion paths, a tiered ranking formula, several feedback loops, and higher-order synthesis layers.

---

## Storage (`memories` table)

Each memory row holds: `summary`, `embedding` (vector(1024)), `salience` (1–10, default 5), `tags`, `memory_type` (general/fact/preference/event/email/calendar/episode/reflection/identity), `entities` (text[]), `confidence` (0–1), `retrieval_count`, `usefulness_score` (0–1, default 0.5), `source`, `superseded_by`, `related_ids`, and a generated `summary_tsv` column for full-text search. There are GIN indexes on tags, entities, and summary_tsv, plus an HNSW index on embeddings (handles continuous inserts/deletes without periodic reindexing, unlike IVFFlat).

---

## Ingestion Paths (4 routes into memory)

| Path | Triage? | Quality Score? | Contradiction Check? | Threshold |
|---|---|---|---|---|
| **Telegram conversation** | Subagent (GBNF) or deep thinker fallback | Subagent (if available) | Yes | salience >= 3 (tools) / >= 5 (parametric) |
| **Email / Calendar check** | Subagent (GBNF) or deep thinker fallback | Subagent (if available) | Yes | All saved |
| **LLM `save_memory` tool** | No | No | No | All saved |
| **Context slide archive** | Subagent distillation (if available) | No | No | Fixed salience=3 (raw) / >= 5 (distilled facts) |

All paths chunk content at **~1200 bytes** (`MaxChunkBytes`) before embedding. BGE-large-en-v1.5 has a 512-token context window; WordPiece tokenization ranges from ~4 bytes/token (plain English) down to ~2.8 bytes/token for structured/HTML content. 1200 bytes stays safely under the 512-token hard limit even for dense email content (~425 tokens at worst case).

**Triage** produces a salience score (1–10), summary, category, and tags. The primary path uses the **subagent** (GLM-4.7-Flash) with a GBNF grammar constraint for structured JSON output, configured via a `TriageConfig` struct. When the subagent is unavailable, triage falls back to the **deep thinker** (GLM-Z1-32B) with thinking disabled. For conversation triage, tool-grounded exchanges use a salience threshold of 3; unverified parametric responses use a threshold of 5 to prevent hallucinated facts from entering memory. The scoring rubric: 1–3 = routine noise, 4–6 = temporal/project relevance, 7–8 = high value/identity, 9–10 = critical/permanent (life-altering only).

**Quality scoring** via the subagent produces specificity, uniqueness, entities, and confidence. A quality boost adjusts salience: `salience += (specificity+uniqueness)/2 * (1 - salience/10)`.

**Contradiction detection** embeds the new memory, finds top-3 similar (cosine distance < 0.3), and asks the subagent "CONTRADICTS or COMPATIBLE?" Contradicted memories get `superseded_by` set to the new memory's ID.

---

## Retrieval (3 paths)

1. **Subconscious prefetch** (every Telegram message, 2s timeout) — Embeds a *trajectory string* built from the last 3 user messages + current message (contextual vector recall), but uses current message alone for BM25/entity matching. Returns top 3 memories injected as background context.

2. **Heartbeat prefetch** (every 5min tick) — Embeds the current task, retrieves top 3 relevant memories for the LLM's heartbeat reasoning.

3. **`search_memory` tool** (LLM-initiated) — Full pipeline: query rewriting (3 subagent variations), multi-embedding retrieval (4 queries x 5 results), entity graph multi-hop (JOIN on shared entities, 0.8x score penalty), deduplication by content hash, subagent re-ranking, and a configurable limit (default 10).

---

## Ranking Formula (`memory.RankingOrderBy`)

A single shared formula used across all 3 prefetch/search paths (lower = better):

```
cosine_distance
  / (1 + BM25_ts_rank * 10)        -- full-text keyword boost
  - salience * 0.1                  -- stored salience (1-10)
  - usefulness_score * 0.15         -- learned usefulness
  - recency * 0.03                  -- ~30-day half-life on creation date
  - confidence * 0.03               -- factual confidence from quality scoring
  - ln(retrieval_count + 1) * 0.02  -- log-dampened popularity
  - entity_exact_match * 0.2        -- bonus when query matches entities array
```

---

## Feedback Loops

- **Retrieval tracking**: Every retrieval bumps `retrieval_count++`, resets `last_retrieved_at`, and applies a dampened salience boost: `salience += 0.3 * (1 - salience/10)`.
- **Usefulness evaluation**: After each Telegram exchange, the subagent evaluates whether prefetched memories contributed to the response (YES/NO). Adjusts `usefulness_score` via dampened curves toward 1.0 (useful) or 0.0 (not useful), step size 0.1.

---

## Decay & Pruning (periodic maintenance in heartbeat)

- **Salience decay**: `salience *= 0.977^days` (~30-day half-life). Floor of 1. Only affects memories not accessed in the last day.
- **Usefulness regression**: Memories not retrieved in 30+ days have `usefulness_score` regressed 5% toward 0.5 per tick, preventing permanently-low scores from blocking retrieval.
- **Pruning**: Memories with `salience <= 1`, older than 90 days (configurable), AND either superseded or never retrieved are deleted.

---

## Higher-Order Synthesis Layers (Event-Driven Cognitive Processing)

Synthesis layers are triggered by **event-driven cognitive processing** (`engine/cognitive.go`) rather than fixed intervals. Cognitive runs fire when: (1) unreflected memory count exceeds `COGNITIVE_BUFFER_THRESHOLD` (default 20), AND (2) the user has been idle for `LULL_DURATION` (default 20min). A hard ceiling (`COGNITIVE_CEILING`, default 4h) forces a run regardless.

| Layer | Trigger | Input | Output |
|---|---|---|---|
| **Consolidation** | Cognitive run or explicit tool call | Top memories with salience >= 8 (tool) or >= 5 (startup) | Updated identity profile row (`memory_type = 'identity'`, salience=10) in DB (thinking disabled). Also produces personality trait updates. |
| **Episodic synthesis** | Cognitive run | Last 24h memories, clustered by cosine similarity (threshold 0.7) | Episode memories (salience=8, type=`episode`) with `related_ids` linking constituents |
| **Reflection** | 50 new memories (checked during cognitive run) | Memories since last reflection, grouped by source/type (min 5 required) | Reflection memory (salience=9, type=`reflection`) analyzing PATTERNS, EVOLVING INTERESTS, CONNECTIONS, PREDICTIONS |

---

## Thinking Mode by Call Site

| Call site | Method | Thinking | Rationale |
|---|---|---|---|
| `consult_deep_thinker` | `Complete` | on | Open-ended reasoning |
| Triage (email/calendar/conversation) | `Triage` (think:false) | **off** | Structured classification |
| Consolidation (profile synthesis) | `CompleteNoThink` | **off** | Structured JSON transformation |
| Episode synthesis | `Complete` (via SynthesizeFunc) | on | Narrative reasoning |
| Reflection | `Complete` (via SynthesizeFunc) | on | Analytical meta-cognition |

---

## Key Constants

| Value | Meaning |
|---|---|
| 1200 bytes | Max chunk size per embedding (`MaxChunkBytes`) |
| 0.977/day | Salience decay rate (~30-day half-life) |
| 3 / 5 | Conversation triage save threshold (tool-grounded / parametric) |
| 8.0 | Episode salience |
| 9.0 | Reflection salience |
| 0.3 | Cosine distance threshold for contradiction + clustering |
| 0.3 | Retrieval salience boost (dampened: `+0.3 * (1 - salience/10)`) |
| 0.8x | Entity hop score penalty |
| 90 days | Default staleness for pruning |
