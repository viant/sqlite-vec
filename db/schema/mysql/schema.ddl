CREATE DATABASE IF NOT EXISTS sqlite_vec
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_unicode_ci;

USE sqlite_vec;

-- Logical datasets/collections managed centrally.
CREATE TABLE IF NOT EXISTS vec_dataset (
    dataset_id   VARCHAR(191) NOT NULL,
    description  TEXT NULL,
    source_uri   TEXT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    last_scn     BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY(dataset_id)
) ENGINE=InnoDB;

-- Template shadow table for documents + embeddings (default name shadow_vec_docs).
CREATE TABLE IF NOT EXISTS shadow_vec_docs (
    id              VARCHAR(191) NOT NULL,
    dataset_id      VARCHAR(191) NOT NULL,
    content         MEDIUMTEXT NULL,
    meta            MEDIUMTEXT NULL,
    embedding       LONGBLOB NOT NULL,
    embedding_model VARCHAR(191) NOT NULL,
    scn             BIGINT NOT NULL,
    archived        TINYINT(1) NOT NULL DEFAULT 0,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY(id),
    KEY idx_shadow_vec_docs_dataset (dataset_id),
    KEY idx_shadow_vec_docs_scn (dataset_id, scn),
    CONSTRAINT fk_shadow_vec_docs_dataset
        FOREIGN KEY (dataset_id) REFERENCES vec_dataset(dataset_id)
        ON DELETE CASCADE
) ENGINE=InnoDB;

-- Change-log to drive replication into SQLite instances.
CREATE TABLE IF NOT EXISTS vec_shadow_log (
    dataset_id   VARCHAR(191) NOT NULL,
    shadow_table VARCHAR(191) NOT NULL,
    scn          BIGINT NOT NULL,
    op           ENUM('insert','update','delete') NOT NULL,
    document_id  VARCHAR(191) NOT NULL,
    payload      LONGBLOB NOT NULL,
    PRIMARY KEY(dataset_id, shadow_table, scn),
    CONSTRAINT fk_vec_shadow_log_dataset
        FOREIGN KEY (dataset_id) REFERENCES vec_dataset(dataset_id)
        ON DELETE CASCADE
) ENGINE=InnoDB;

-- Optional central record of downstream checkpoints.
CREATE TABLE IF NOT EXISTS vec_sync_state (
    dataset_id   VARCHAR(191) NOT NULL,
    shadow_table VARCHAR(191) NOT NULL,
    last_scn     BIGINT NOT NULL,
    PRIMARY KEY(dataset_id, shadow_table)
) ENGINE=InnoDB;

-- Persisted vector indexes keyed by shadow table + dataset.
CREATE TABLE IF NOT EXISTS vector_storage (
    shadow_table_name VARCHAR(191) NOT NULL,
    dataset_id        VARCHAR(191) NOT NULL DEFAULT '',
    `index`           LONGBLOB NULL,
    PRIMARY KEY(shadow_table_name, dataset_id)
) ENGINE=InnoDB;
