CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS memories (
    id         BIGSERIAL PRIMARY KEY,
    summary    TEXT NOT NULL,
    embedding  VECTOR(1024) NOT NULL,
    salience   FLOAT DEFAULT 5,
    tags          TEXT[],
    created_at    TIMESTAMPTZ DEFAULT now(),
    last_accessed TIMESTAMPTZ DEFAULT now()
);

-- HNSW handles continuous inserts/deletes without periodic reindexing,
-- unlike IVFFlat which requires REINDEX as data distribution shifts.
CREATE INDEX IF NOT EXISTS memories_embedding_hnsw_idx
    ON memories USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- Add last_accessed column for salience decay (safe to re-run).
ALTER TABLE memories ADD COLUMN IF NOT EXISTS last_accessed TIMESTAMPTZ DEFAULT now();

CREATE INDEX IF NOT EXISTS memories_tags_idx ON memories USING gin (tags);

-- Memory system upgrades: retrieval tracking, structured writes, contradiction detection.
ALTER TABLE memories ADD COLUMN IF NOT EXISTS memory_type VARCHAR(20) DEFAULT 'general';
ALTER TABLE memories ADD COLUMN IF NOT EXISTS entities TEXT[];
ALTER TABLE memories ADD COLUMN IF NOT EXISTS confidence FLOAT DEFAULT 1.0;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS related_ids BIGINT[];
ALTER TABLE memories ADD COLUMN IF NOT EXISTS superseded_by BIGINT REFERENCES memories(id);
ALTER TABLE memories ADD COLUMN IF NOT EXISTS retrieval_count INT DEFAULT 0;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS last_retrieved_at TIMESTAMPTZ;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS usefulness_score FLOAT DEFAULT 0.5;

CREATE INDEX IF NOT EXISTS memories_superseded_idx ON memories (id) WHERE superseded_by IS NULL;
CREATE INDEX IF NOT EXISTS memories_type_idx ON memories (memory_type);
CREATE INDEX IF NOT EXISTS memories_entities_idx ON memories USING gin (entities);

ALTER TABLE memories ADD COLUMN IF NOT EXISTS source VARCHAR(50);
CREATE INDEX IF NOT EXISTS memories_source_idx ON memories (source);

ALTER TABLE memories ADD COLUMN IF NOT EXISTS source_date TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS memories_source_date_idx ON memories (source_date);

-- Stored generated column + GIN index for full-text search on summary.
ALTER TABLE memories ADD COLUMN IF NOT EXISTS summary_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('english', summary)) STORED;
CREATE INDEX IF NOT EXISTS memories_summary_tsv_idx ON memories USING gin (summary_tsv);

CREATE TABLE IF NOT EXISTS processed_emails (
    gmail_id     TEXT PRIMARY KEY,
    processed_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS processed_events (
    event_id     TEXT PRIMARY KEY,
    processed_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tasks (
    id          BIGSERIAL PRIMARY KEY,
    description TEXT NOT NULL,
    due_at      TIMESTAMPTZ,
    recurrence  BIGINT DEFAULT 0,  -- nanoseconds (maps directly to Go time.Duration)
    status      VARCHAR(20) DEFAULT 'pending',
    created_at  TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tasks_pending_due_idx
    ON tasks (due_at ASC)
    WHERE status = 'pending' AND due_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS directives (
    id                SERIAL PRIMARY KEY,
    name              VARCHAR(255) UNIQUE NOT NULL,
    interval_duration INTERVAL NOT NULL,
    last_executed     TIMESTAMPTZ DEFAULT now(),
    instruction       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS backfill_runs (
    kind         VARCHAR(20) NOT NULL,
    query        TEXT NOT NULL,
    completed_at TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (kind, query)
);

CREATE TABLE IF NOT EXISTS user_preferences (
    key        VARCHAR(255) PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now()
);
