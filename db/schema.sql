CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS memories (
    id BIGSERIAL PRIMARY KEY,
    summary TEXT NOT NULL,
    embedding VECTOR (1024) NOT NULL,
    salience FLOAT DEFAULT 5,
    tags TEXT [],
    created_at TIMESTAMPTZ DEFAULT now(),
    last_accessed TIMESTAMPTZ DEFAULT now()
);

-- HNSW handles continuous inserts/deletes without periodic reindexing,
-- unlike IVFFlat which requires REINDEX as data distribution shifts.
CREATE INDEX IF NOT EXISTS memories_embedding_hnsw_idx ON memories USING hnsw (embedding vector_cosine_ops)
WITH (m = 16, ef_construction = 64);

-- Add last_accessed column for salience decay (safe to re-run).
ALTER TABLE memories
ADD COLUMN IF NOT EXISTS last_accessed TIMESTAMPTZ DEFAULT now();

CREATE INDEX IF NOT EXISTS memories_tags_idx ON memories USING gin (tags);

-- Memory system upgrades: retrieval tracking, structured writes, contradiction detection.
ALTER TABLE memories
ADD COLUMN IF NOT EXISTS memory_type VARCHAR(20) DEFAULT 'general';

ALTER TABLE memories ADD COLUMN IF NOT EXISTS entities TEXT [];

ALTER TABLE memories
ADD COLUMN IF NOT EXISTS confidence FLOAT DEFAULT 1.0;

ALTER TABLE memories ADD COLUMN IF NOT EXISTS related_ids BIGINT[];

ALTER TABLE memories
ADD COLUMN IF NOT EXISTS superseded_by BIGINT REFERENCES memories (id);

ALTER TABLE memories
ADD COLUMN IF NOT EXISTS retrieval_count INT DEFAULT 0;

ALTER TABLE memories
ADD COLUMN IF NOT EXISTS last_retrieved_at TIMESTAMPTZ;

ALTER TABLE memories
ADD COLUMN IF NOT EXISTS usefulness_score FLOAT DEFAULT 0.5;

CREATE INDEX IF NOT EXISTS memories_superseded_idx ON memories (id)
WHERE
    superseded_by IS NULL;

CREATE INDEX IF NOT EXISTS memories_type_idx ON memories (memory_type);

CREATE INDEX IF NOT EXISTS memories_entities_idx ON memories USING gin (entities);

ALTER TABLE memories ADD COLUMN IF NOT EXISTS source VARCHAR(50);

CREATE INDEX IF NOT EXISTS memories_source_idx ON memories (source);

ALTER TABLE memories
ADD COLUMN IF NOT EXISTS source_date TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS memories_source_date_idx ON memories (source_date);

-- Stored generated column + GIN index for full-text search on summary.
ALTER TABLE memories
ADD COLUMN IF NOT EXISTS summary_tsv tsvector GENERATED ALWAYS AS (
    to_tsvector('english', summary)
) STORED;

CREATE INDEX IF NOT EXISTS memories_summary_tsv_idx ON memories USING gin (summary_tsv);

-- Legacy tasks table: migrated to work_items on startup (see migrateTasksTable).
-- CREATE TABLE kept for reference only; new installs use work_items directly.

-- Legacy migration: rename old table name if it exists.
ALTER TABLE IF EXISTS directives RENAME TO routines;

CREATE TABLE IF NOT EXISTS routines (
    id SERIAL PRIMARY KEY,
    name VARCHAR(255) UNIQUE NOT NULL,
    interval_duration INTERVAL,          -- nullable: schedule-only routines have no interval
    last_executed TIMESTAMPTZ DEFAULT now(),
    instruction TEXT,                     -- nullable: structured routines use tool+goal instead
    tool VARCHAR(255),                    -- single tool to call directly
    tools TEXT[],                         -- multi-tool list (takes precedence over tool)
    tool_args JSONB,                      -- per-tool arguments with template expansion
    goal TEXT,                            -- what to do with tool results
    silent_if_empty BOOLEAN DEFAULT false,-- skip orchestrator if tool returns empty
    schedule TEXT                         -- "HH:MM" or comma-separated "HH:MM,HH:MM" daily schedule
);

-- Migrations for existing databases (safe to re-run).
ALTER TABLE routines ADD COLUMN IF NOT EXISTS tool VARCHAR(255);
ALTER TABLE routines ADD COLUMN IF NOT EXISTS goal TEXT;
ALTER TABLE routines ADD COLUMN IF NOT EXISTS silent_if_empty BOOLEAN DEFAULT false;
ALTER TABLE routines ADD COLUMN IF NOT EXISTS schedule TEXT;
ALTER TABLE routines ADD COLUMN IF NOT EXISTS tools TEXT[];
ALTER TABLE routines ALTER COLUMN interval_duration DROP NOT NULL;
ALTER TABLE routines ALTER COLUMN instruction DROP NOT NULL;
ALTER TABLE routines ADD COLUMN IF NOT EXISTS tool_args JSONB;

CREATE TABLE IF NOT EXISTS user_preferences (
    key VARCHAR(255) PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS personality_traits (
    id SERIAL PRIMARY KEY,
    category VARCHAR(50) NOT NULL,
    trait_key VARCHAR(255) NOT NULL,
    trait_value TEXT NOT NULL,
    context TEXT,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE (category, trait_key)
);

CREATE INDEX IF NOT EXISTS personality_traits_category_idx ON personality_traits (category);

CREATE TABLE IF NOT EXISTS processed_emails (
    message_id VARCHAR(255) PRIMARY KEY,
    seen_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS processed_emails_meta (
    key VARCHAR(64) PRIMARY KEY,
    checked_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS processed_events (
    event_uid VARCHAR(512) PRIMARY KEY,
    seen_at TIMESTAMPTZ DEFAULT now()
);

-- Routine seed data removed: routines.toml is the source of truth.
-- SyncFromFile() on startup upserts all TOML entries and deletes any DB
-- routines not present in the file.

CREATE TABLE IF NOT EXISTS conversation_snapshot (
    id INTEGER PRIMARY KEY DEFAULT 1,
    messages JSONB NOT NULL DEFAULT '[]',
    updated_at TIMESTAMPTZ DEFAULT now()
);

-- Unified work tracking: replaces both tasks and background_tasks tables.
-- Rename existing background_tasks if upgrading (idempotent: skipped if already renamed).
ALTER TABLE IF EXISTS background_tasks RENAME TO work_items;

CREATE TABLE IF NOT EXISTS work_items (
    id BIGSERIAL PRIMARY KEY,
    type VARCHAR(20) NOT NULL DEFAULT 'background', -- 'scheduled', 'background', 'routine'
    directive TEXT NOT NULL,
    status VARCHAR(20) DEFAULT 'pending',            -- pending, running, completed, failed, cancelled
    result TEXT,
    steps_total INT DEFAULT 0,
    steps_completed INT DEFAULT 0,
    error_message TEXT,
    created_at TIMESTAMPTZ DEFAULT now(),
    completed_at TIMESTAMPTZ,
    priority INT DEFAULT 5,
    due_at TIMESTAMPTZ,              -- scheduled tasks: when to fire
    recurrence BIGINT DEFAULT 0,     -- scheduled tasks: nanoseconds between recurrences
    started_at TIMESTAMPTZ,          -- when execution began (all types)
    timeout_at TIMESTAMPTZ           -- watchdog: kill if still running past this time
);

-- Migrations for existing databases (safe to re-run).
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS type VARCHAR(20) NOT NULL DEFAULT 'background';
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS priority INT DEFAULT 5;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS due_at TIMESTAMPTZ;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS recurrence BIGINT DEFAULT 0;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS timeout_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS work_items_running_idx ON work_items (status) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS work_items_scheduled_idx ON work_items (due_at ASC)
    WHERE type = 'scheduled' AND status = 'pending' AND due_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS work_items_timeout_idx ON work_items (timeout_at)
    WHERE status = 'running' AND timeout_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS failed_operations (
    id BIGSERIAL PRIMARY KEY,
    op_type VARCHAR(50) NOT NULL,
    label TEXT NOT NULL,
    error_message TEXT NOT NULL,
    context_data JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_failed_operations_created ON failed_operations (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_failed_operations_type ON failed_operations (op_type);

CREATE TABLE IF NOT EXISTS skill_kv (
    skill_name TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (skill_name, key)
);
