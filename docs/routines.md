# Routine System

Routines are persistent background habits that execute autonomously during the heartbeat loop. They are defined in `.config/routines.toml` (source of truth), synced to PostgreSQL (runtime cache), and managed by the `routines/` package.

---

## Package Structure

```
routines/
  routines.go     # Entry, DueRoutine, NilIfEmpty, IsEmptyResult
  schedule.go     # NormalizeSchedule, ParseSchedules, ValidateSchedules, IsScheduleDue
  args.go         # ExpandArgs, ExpandAndMarshal, ExpandString (template expansion)
  file.go         # FileWriter interface, FileAdapter, LoadFile
  sync.go         # SyncFromFile, SyncIfChanged, Upsert, Delete, QueryDue, AdvanceTimer
```

---

## TOML Format

### Structured Routine (preferred)

```toml
[feed-digest]
interval = "4 hours"
action = "scan_feeds"
goal = "Select 3-5 most interesting items. Send a digest."
silent_if_empty = true
```

The engine calls the action directly, then passes the result + `goal` to the orchestrator. If `silent_if_empty = true` and the action returns no data, the orchestrator is skipped.

### Multi-Action Routine

```toml
[morning-briefing]
schedule = "06:00"
actions = ["search_email", "get_weather", "search_calendar"]
goal = "Synthesize everything into a concise daily orientation."
```

Actions are called in order. Results are concatenated and passed to the orchestrator as a single prompt.

---

## Action Arguments

`action_args` provides per-action arguments. Each key under `[routine-name.action_args.action-name]` becomes a JSON argument passed to that action at execution time.

```toml
[morning-briefing.action_args.search_calendar]
time_min = "{{today}}"
time_max = "{{tomorrow}}"

[morning-briefing.action_args.search_email]
max_results = 10
```

Arguments can be any type:
- **Strings** — passed through, with template expansion if `{{...}}` is present
- **Numbers** — passed as-is
- **Booleans** — passed as-is

### Template Expansion

String values containing `{{...}}` expressions are expanded at execution time using the local timezone.

| Template | Expands to | Example |
|----------|-----------|---------|
| `{{today}}` | Today 00:00 | `2026-03-02T00:00:00` |
| `{{tomorrow}}` | Tomorrow 00:00 | `2026-03-03T00:00:00` |
| `{{yesterday}}` | Yesterday 00:00 | `2026-03-01T00:00:00` |
| `{{now}}` | Current time | `2026-03-02T14:30:00` |

**Relative offsets** can be applied to any base keyword:

| Offset | Example | Result |
|--------|---------|--------|
| `+Nm` / `-Nm` | `{{now-30m}}` | 30 minutes ago |
| `+Nh` / `-Nh` | `{{now-2h}}` | 2 hours ago |
| `+Nd` / `-Nd` | `{{today+3d}}` | 3 days from midnight |
| `+Nw` / `-Nw` | `{{yesterday-1w}}` | 8 days ago at midnight |

Templates are expanded by `routines.ExpandAndMarshal()` in `engine/routines.go` just before the action is called. The expansion is regex-based (`{{keyword[+-]offset}}`), so templates can appear anywhere in a string value, including embedded in larger strings.

---

## Triggers

### Interval

```toml
interval = "4 hours"
```

Fires when the duration elapses since last execution. Stored as a PostgreSQL `INTERVAL` in the `interval_duration` column. The DB query filters for `last_executed + interval_duration <= NOW()`.

### Schedule

```toml
schedule = "06:00"           # single time
schedule = ["06:00", "18:00"] # multiple times
```

Fires at specific daily times (local timezone). Each time is checked independently — the routine fires if ANY time has passed today AND `last_executed` was before that time.

Multi-time schedules are stored as a comma-separated string in the DB (`"06:00,18:00"`). `NormalizeSchedule()` converts the TOML `interface{}` (string or `[]string`) to this format. `ParseSchedules()` splits it back.

### Combined

```toml
interval = "8 hours"
schedule = "06:00"
```

Both triggers are allowed on the same routine. `QueryDue()` returns the routine if **either** trigger fires. This is useful for "at least once in the morning + every N hours" patterns.

---

## Execution Flow

Routines run on their own independent scheduler (`runRoutineScheduler`), decoupled from the heartbeat loop. The routine scheduler polls at `ROUTINE_INTERVAL` (default 30s), giving routines much better time precision than the heartbeat interval (default 5m).

1. **`runRoutineScheduler()`** ticks at `RoutineInterval` (default 30s)
   - Calls `RoutineSyncFunc()` to hot-reload `.config/routines.toml` (mtime-based, very fast)
   - Calls `executeDueRoutines()`
2. **`executeDueRoutines()`** calls `routines.QueryDue(pool)` which:
   - Queries routines where interval has elapsed OR schedule is non-null
   - Parses schedules and action_args from DB columns
   - Filters schedule-based routines via `IsScheduleDue()`
   - Returns `[]DueRoutine`
3. **`executeSingleRoutine(d)`** for each due routine:
   - Calls `routines.AdvanceTimer()` (prevents double-fire on crash)
   - Resolves action list: `Actions` > `Action` > legacy `Instruction`
   - For each action: looks up `d.ActionArgs[actionName]`, expands via `ExpandAndMarshal()`, calls `ToolExec`
   - Checks `IsEmptyResult()` on each action result
   - If `SilentIfEmpty` and all actions empty, returns silently
   - Otherwise passes concatenated results + goal to the orchestrator

---

## Sync Paths

### 1. Startup

```go
routines.SyncFromFile(db.Pool, ".config/routines.toml")
```

Full sync: reads TOML, upserts all entries, deletes DB routines not in the file. TOML is the source of truth.

### 2. Heartbeat (mtime-based)

```go
routines.SyncIfChanged(db.Pool, ".config/routines.toml", &routineMtime)
```

Checks file mtime on each heartbeat tick. If changed, triggers a full `SyncFromFile`.

### 3. `/reload` Command

```go
routines.SyncFromFile(db.Pool, ".config/routines.toml")
```

Telegram command that forces immediate sync (plus skill hot-reload).

### 4. `manage_routines` Tool (write-back)

The orchestrator can create/update/delete routines via the `manage_routines` tool. Changes are written to both the DB and `.config/routines.toml` via `routines.FileWriter`.

---

## Database Schema

```sql
CREATE TABLE routines (
    id SERIAL PRIMARY KEY,
    name VARCHAR(255) UNIQUE NOT NULL,
    interval_duration INTERVAL,           -- nullable for schedule-only
    last_executed TIMESTAMPTZ DEFAULT now(),
    instruction TEXT,                      -- nullable for structured routines
    action VARCHAR(255),
    actions TEXT[],
    action_args JSONB,                     -- {"action_name": {"key": "value"}}
    goal TEXT,
    silent_if_empty BOOLEAN DEFAULT false,
    schedule TEXT                           -- "HH:MM" or "HH:MM,HH:MM"
);
```

---

## Types

### `routines.Entry`

Unified type for TOML, JSON, and DB representation. `Schedule` is `interface{}` to accept both `"06:00"` (string) and `["06:00", "18:00"]` (array) from TOML.

### `routines.DueRoutine`

Execution candidate with parsed fields:
- `ActionArgs map[string]json.RawMessage` — per-action JSON args from the JSONB column
- `Schedules []string` — parsed from comma-separated schedule column
- `Action *string`, `Goal *string` — nullable DB fields

### `routines.FileWriter`

Interface for TOML write-back:
```go
type FileWriter interface {
    Write(name string, entry Entry) error
    Delete(name string)
}
```

Implemented by `routines.FileAdapter` which serializes to TOML with a mutex-protected read-modify-write cycle.
