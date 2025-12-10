PRAGMA foreign_keys = ON;

-- Datasets represent logical collections (e.g., filesystem roots, tenants, projects).
CREATE TABLE IF NOT EXISTS vec_dataset (
    dataset_id TEXT PRIMARY KEY,
    description TEXT NULL,
    source_uri  TEXT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_scn    INTEGER NOT NULL DEFAULT 0
);

-- Shadow table template: duplicate per virtual table (default name shadow_vec_docs).
-- Documents are already chunked by the upstream ingestion project.
CREATE TABLE IF NOT EXISTS shadow_vec_docs (
    id              TEXT PRIMARY KEY,
    dataset_id      TEXT NOT NULL,
    content         TEXT,
    meta            TEXT,       -- JSON metadata supplied by ingestion pipeline
    embedding       BLOB NOT NULL,
    embedding_model TEXT NOT NULL,
    scn             INTEGER NOT NULL,
    archived        INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(dataset_id) REFERENCES vec_dataset(dataset_id)
);
CREATE INDEX IF NOT EXISTS idx_vec_docs_dataset
    ON shadow_vec_docs(dataset_id);
CREATE INDEX IF NOT EXISTS idx_vec_docs_scn
    ON shadow_vec_docs(dataset_id, scn);

-- Change-log consumed by local SQLite replicas to replay SCNs.
CREATE TABLE IF NOT EXISTS vec_shadow_log (
    dataset_id   TEXT NOT NULL,
    shadow_table TEXT NOT NULL,
    scn          INTEGER NOT NULL,
    op           TEXT NOT NULL,
    document_id  TEXT NOT NULL,
    payload      BLOB NOT NULL,
    PRIMARY KEY(dataset_id, shadow_table, scn),
    FOREIGN KEY(dataset_id) REFERENCES vec_dataset(dataset_id)
);

-- Local sync checkpoints per dataset/shadow pair.
CREATE TABLE IF NOT EXISTS vec_sync_state (
    dataset_id   TEXT NOT NULL,
    shadow_table TEXT NOT NULL,
    last_scn     INTEGER NOT NULL,
    PRIMARY KEY(dataset_id, shadow_table)
);

-- Persisted vector indexes keyed by shadow table + dataset.
CREATE TABLE IF NOT EXISTS vector_storage (
    shadow_table_name TEXT NOT NULL,
    dataset_id        TEXT NOT NULL DEFAULT '',
    "index"           BLOB,
    PRIMARY KEY (shadow_table_name, dataset_id)
);
