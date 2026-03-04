# Dispatch Architecture

The dispatch system routes user messages through a tiered architecture where the 9B model is always the user-facing layer. The Brain handles complex reasoning silently in the background when needed.

---

## Message Flow

```
User message (Telegram)
  → Prefetch (memory retrieval, temporal context)
  → 9B Triage (grammar-constrained dispatch decision)
    │
    ├─ dispatch: true (single tool)
    │    → Send ack to user ("Sure, checking...")
    │    → Execute tool
    │    → 9B synthesis → single clean reply
    │
    ├─ dispatch: true, multi: true
    │    → Send ack to user
    │    → SubagentSupervisor loop (2-5 tool rounds)
    │    → Final response from supervisor
    │
    └─ dispatch: false (escalate)
         → Send ack to user ("Let me think about that...")
         → Slot router acquires Brain or 9B
         → Brain reasoning + tool calls
         → Brain's final response sent directly to user
```

---

## Triage (Grammar-Constrained)

The 9B runs dispatch triage via `TryCompleteWithGrammar` — a non-blocking call that returns an error if no subagent slot is available (clean escalation to Brain).

### Grammar (`grammar/grammar.go` — `BuildDispatchGrammar`)

```
root ::= escalate | dispatch | multi

escalate  ::= {"dispatch": false, "ack": "<text>"}
dispatch  ::= {"dispatch": true, "tool": "<name>", "args": {...}, "ack": "<text>"}
multi     ::= {"dispatch": true, "multi": true, "directive": "<text>", "ack": "<text>"}
```

All three branches include an `ack` field — a brief natural reply shown to the user immediately while the tool executes or the Brain thinks.

### Dispatch Rules

1. **Dispatch (single tool)** when the request is a straightforward data fetch that one tool can answer.
2. **Dispatch (multi-step)** when the request needs 2-3 sequential tool calls but no complex reasoning.
3. **Escalate** when the request needs judgment, creativity, complex reasoning, or involves side effects.

Never dispatched: `send_email`, `create_event`, `create_skill`, `manage_skills`, `manage_routines`, `manage_personality`, `save_memory`, `forget_topic`, `consult_deep_thinker`, `plan_and_execute`, `delegate_task`, `ask_database`.

---

## Single-Tool Dispatch

1. Triage returns `{dispatch: true, tool, args, ack}`
2. Ack is sent to user
3. Tool executes with a progress ticker (every 20s: "Still working on X... (Ns)")
4. On success → 9B synthesis
5. On hard/soft error → escalation to Brain with error context

### Escalation on Failure

`dispatchEscalation` captures context from a failed dispatch:
- `ToolName` — which tool failed
- `Phase` — `"triage"` | `"execution"` | `"synthesis"` | `"multi-step"`
- `Error` — error description
- `ToolResult` — when synthesis failed after a successful tool call, the tool result is passed to the Brain to avoid re-execution

---

## Multi-Step Dispatch

When triage returns `{dispatch: true, multi: true, directive, ack}`, the request is handled by `SubagentSupervisor` — a grammar-constrained multi-turn loop where the subagent calls tools and produces a final response.

- Max 5 rounds, 90s overall timeout
- Tool errors get 3 free retries (don't count against round budget)
- On failure, escalates to Brain

---

## Brain Escalation

When triage returns `{dispatch: false, ack}`:

1. The ack is sent to the user immediately (latency win — user sees "Let me think about that" in <1s)
2. The slot router acquires a Brain or 9B orchestrator slot
3. The Brain runs the full supervisor loop (tool intents, tool execution, multi-round reasoning)
4. The Brain's intermediate prose (alongside tool intents) is **discarded** — the system prompt tells the Brain that only the final response reaches the user
5. The Brain's final response is sent directly — no synthesis layer, avoiding added latency

The system prompt instructs the Brain: "Your final message is the only one the user receives." This ensures the Brain writes a complete response in its last round instead of assuming the user saw earlier prose.

---

## Slot Router

The slot router (`engine/slot_router.go`) decides which LLM backend handles an orchestrator call.

### Two-Model Mode

| Backend | Slots | Use Case |
|---------|-------|----------|
| Brain (122B) | 1 x 32K | Interactive messages (preferred), deep reasoning |
| 9B | 3 x 16K | Routines, heartbeats, dispatch triage/synthesis |

### Routing Strategies

**Interactive** (`preferBrain=true`):
```
Try Brain (non-blocking) → Try 9B (non-blocking) → Block on Brain
```

**Background** (`preferBrain=false`):
```
Try 9B (non-blocking) → Block on Brain
```

### Slot Lifecycle

During tool execution, the orchestrator slot is released (`OnToolStart`) and reacquired after (`OnToolEnd`). This prevents tools (which may take minutes for web fetches, email searches, etc.) from holding a precious LLM slot idle.

---

## Synthesis (Dispatch Path Only)

Post-tool synthesis in the dispatch path uses `buildSynthesisPrompt()`:

1. Personality content (voice/tone)
2. Core instruction: "Present results naturally as if you already knew them"
3. User profile (context about the user)
4. Prefetched memories (relevant context)
5. Temporal context (current time, upcoming events)

### Synthesis Fallback Chain

1. **9B subagent** `Complete()` — 30s timeout
2. **DTC** `CompleteNoThink()` — 45s timeout (lightweight, no thinking overhead)
3. **Escalate to Brain** — if both fail, dispatch escalates with tool result attached

Brain escalation does **not** use synthesis — the Brain's final response is sent directly to avoid added latency.

---

## Key Constants

| Constant | Value | Purpose |
|----------|-------|---------|
| `timeoutDispatchTriage` | 5s | Triage grammar call |
| `timeoutDispatchToolExec` | 5min | Single tool execution |
| `timeoutDispatchSynthesis` | 30s | 9B synthesis |
| `timeoutDispatchDTCSynthesis` | 45s | DTC synthesis fallback |
| `timeoutMultiStepDispatch` | 90s | Multi-step supervisor loop |
| `dispatchMaxTriageTokens` | 512 | Triage output token limit |
| `dispatchMaxSynthTokens` | 2048 | Synthesis output token limit |
| `dispatchMaxResultLen` | 8000 | Tool result truncation for synthesis input |
| `dispatchProgressInterval` | 20s | Progress update frequency |
| `maxMultiStepRounds` | 5 | Multi-step tool call rounds |
