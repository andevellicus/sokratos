# Orchestrator Architecture

The orchestrator routes user messages through a grammar-constrained LLM loop where all tool calls and responses are valid JSON enforced by GBNF grammar.

---

## Message Flow

```
User message (Telegram)
  → Slot router acquires Brain or 9B (AcquireOrFallback)
  → Prefetch (memory retrieval + temporal context) in background
  → Grammar-constrained orchestrator loop (runGrammarOrchestrator)
    │
    ├─ {"action":"tool","name":"...","arguments":{...}}
    │    → Execute tool
    │    → Inject result as user message
    │    → Next round (max 15 rounds)
    │
    ├─ {"action":"respond","text":"..."}
    │    → Send final response to user
    │
    └─ MandatedBrainTools intercept (create_skill, update_skill)
         → BackgroundJobRequest → runBackgroundJob
```

---

## Grammar-Constrained Orchestrator

The core loop lives in `llm/orchestrator.go` (`runGrammarOrchestrator`). The GBNF grammar forces the LLM to produce either:
- `{"action":"tool","name":"<tool>","arguments":{...}}` — execute a tool
- `{"action":"respond","text":"..."}` — final response to user

The grammar is built from all registered tool schemas via `BuildSubagentToolGrammar()` in `grammar/subagent_tool_grammar.go`. The built-in "respond" schema is filtered out since the grammar has its own respond rule. Grammar is rebuilt whenever skills are hot-reloaded.

### Thinking Mode

The orchestrator supports optional chain-of-thought reasoning alongside grammar constraints via two modes on `QueryOrchestratorOpts`:
- `EnableThinking` — thinking on all rounds (used by Brain background jobs)
- `FirstRoundThinking` — thinking on round 0 only (interactive orchestrator)

When thinking is enabled for a round:
1. If a GBNF grammar is active, it's wrapped via `textutil.WrapGrammarWithThinkBlock()` to require a mandatory `<think>...</think>` prefix before the JSON output. `reasoning_format` is set to `"none"` (llama-server's `"deepseek"` mode conflicts with GBNF).
2. If no grammar is active (grammar-free Brain calls), `reasoning_format: "deepseek"` is used instead, which separates thinking into `reasoning_content` server-side.

**Reasoning preservation across rounds**: Qwen3.5's Jinja chat template strips `<think>` blocks from historical messages. The `processThinking()` helper converts think content to a visible `[Reasoning: ...]` prefix in stored messages, so subsequent rounds can reference prior reasoning without extra inference cost.

### Error Handling

- Tool soft errors (returned as string, nil) are injected back as tool results — the orchestrator can retry or explain
- Parse failures get up to 3 free retries (defense-in-depth for grammar bypass edge cases)
- Tool execution errors get up to 3 retries with corrective hints
- Fallback chains (`FallbackMap`) allow deterministic retry with a different tool on failure

---

## Slot Router

The slot router (`engine/slot_router.go`) decides which LLM backend handles an orchestrator call.

### Two-Model Mode

| Backend | Slots | Use Case |
|---------|-------|----------|
| 9B | 3 x 16K | All orchestration (interactive, routines, heartbeats, subagent tasks) |
| Brain (122B) | 1 x 32K | Fallback when 9B busy, background Brain jobs (deep_think, create_skill) |

### Routing Strategies

**All orchestration** (`preferBrain=false`):
```
Try 9B (non-blocking) → Block on Brain
```
The 9B handles all grammar-constrained orchestration. When deep reasoning is needed, it calls `deep_think` or `consult_deep_thinker`.

**Background Brain jobs** (`preferBrain=true`):
```
Try Brain (non-blocking) → Try 9B (non-blocking) → Block on Brain
```

### Slot Lifecycle

During tool execution, the orchestrator slot is released (`OnToolStart`) and reacquired after (`OnToolEnd`). This prevents tools (which may take minutes for web fetches, email searches, etc.) from holding a precious LLM slot idle.

---

## Background Brain Jobs

Complex tasks are offloaded to background Brain sessions (`runBackgroundJob` in `dispatch.go`) that run concurrently. Two paths:

1. **Mandatory intercept** — `create_skill` and `update_skill` are intercepted at the orchestrator level via `MandatedBrainTools` and always routed to a background Brain session.
2. **Voluntary** — The orchestrator calls `deep_think(background=true)` which returns a `BackgroundJobRequest` sentinel error, propagated through the orchestrator to `processMessage`.

Each job selects a session prompt by `TaskType` (`brainSessionPrompts` map), falling back to `prompts.SessionReason` for general tasks. Jobs support multi-round tool execution (max 20 rounds) and can ask the user questions — the goroutine parks waiting for input via `reply_to_job`. The `toolSucceeded` flag provides a completion signal for jobs with a target tool (e.g. `create_skill`).

---

## Code Location

- `llm/orchestrator.go` — grammar-constrained orchestrator loop (`runGrammarOrchestrator`, `buildOrchestratorSystemPrompt`, `processThinking`)
- `llm/tools.go` — shared utilities (`IsToolSoftError`, `matchFallback`, `buildToolJSON`, `toolHint`)
- `textutil/textutil.go` — `WrapGrammarWithThinkBlock`, `StripThinkTags`, `ExtractThinkContent`
- `llm/client.go` — `QueryOrchestrator` entry point, `ToolAgentConfig`, `QueryOrchestratorOpts`
- `dispatch.go` — background Brain jobs (`runBackgroundJob`), session prompts, mandated brain tools
- `message_loop.go` — `processMessage`, `completeMessageHandling`, command handlers
