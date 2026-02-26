-- NOTE: The canonical schema lives in db/schema.sql.
-- This file is a copy for Docker's first-run initialization only.
-- The Go app re-applies db/schema.sql on every startup via ensureSchema,
-- so this file is purely a convenience for fresh Docker volumes.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS memories (
    id         BIGSERIAL PRIMARY KEY,
    summary    TEXT NOT NULL,
    embedding  VECTOR(1024) NOT NULL,
    salience   FLOAT DEFAULT 5,
    tags       TEXT[],
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS memories_embedding_idx
    ON memories USING ivfflat (embedding vector_cosine_ops)
    WITH (lists = 100);
