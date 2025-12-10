-- MySQL schema for dataset-aware shadow tables with SCN-based synchronization.
-- Apply this script to your upstream (authoritative) database.

CREATE DATABASE IF NOT EXISTS sqlite_vec
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_unicode_ci;

USE sqlite_vec;

-- ------------------------------------------------------------------
-- 1. Dataset registry & SCN allocator
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS vec_dataset (
    dataset_id   VARCHAR(191) NOT NULL,
    description  TEXT NULL,
    source_uri   TEXT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    last_scn     BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY(dataset_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS vec_dataset_scn (
    dataset_id VARCHAR(191) NOT NULL,
    next_scn   BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY(dataset_id),
    CONSTRAINT fk_dataset_scn_dataset
        FOREIGN KEY (dataset_id) REFERENCES vec_dataset(dataset_id)
        ON DELETE CASCADE
) ENGINE=InnoDB;

DROP FUNCTION IF EXISTS vec_next_scn;
DELIMITER $$
CREATE FUNCTION vec_next_scn(p_dataset VARCHAR(191)) RETURNS BIGINT
BEGIN
    DECLARE v_next BIGINT;
    INSERT INTO vec_dataset_scn(dataset_id, next_scn)
    VALUES (p_dataset, 2)
    ON DUPLICATE KEY UPDATE next_scn = next_scn + 1;
    SELECT next_scn INTO v_next FROM vec_dataset_scn WHERE dataset_id = p_dataset;
    UPDATE vec_dataset SET last_scn = v_next WHERE dataset_id = p_dataset;
    RETURN v_next;
END$$
DELIMITER ;

-- ------------------------------------------------------------------
-- 2. Shadow documents + change log
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS shadow_vec_docs (
    dataset_id      VARCHAR(191) NOT NULL,
    id              VARCHAR(191) NOT NULL,
    content         MEDIUMTEXT NULL,
    meta            MEDIUMTEXT NULL,
    embedding       LONGBLOB NOT NULL,
    embedding_model VARCHAR(191) NOT NULL,
    scn             BIGINT NOT NULL,
    archived        TINYINT(1) NOT NULL DEFAULT 0,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY(dataset_id, id),
    KEY idx_vec_docs_scn (dataset_id, scn),
    CONSTRAINT fk_shadow_docs_dataset
        FOREIGN KEY (dataset_id) REFERENCES vec_dataset(dataset_id)
        ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS vec_shadow_log (
    dataset_id   VARCHAR(191) NOT NULL,
    shadow_table VARCHAR(191) NOT NULL,
    scn          BIGINT NOT NULL,
    op           ENUM('insert','update','delete') NOT NULL,
    document_id  VARCHAR(191) NOT NULL,
    payload      LONGBLOB NOT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY(dataset_id, shadow_table, scn),
    CONSTRAINT fk_shadow_log_dataset
        FOREIGN KEY (dataset_id) REFERENCES vec_dataset(dataset_id)
        ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS vec_sync_state (
    dataset_id   VARCHAR(191) NOT NULL,
    shadow_table VARCHAR(191) NOT NULL,
    last_scn     BIGINT NOT NULL,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY(dataset_id, shadow_table)
) ENGINE=InnoDB;

-- Persisted vector indexes (used by sqlite-vec replicas; optional upstream).
CREATE TABLE IF NOT EXISTS vector_storage (
    shadow_table_name VARCHAR(191) NOT NULL,
    dataset_id        VARCHAR(191) NOT NULL DEFAULT '',
    `index`           LONGBLOB NULL,
    PRIMARY KEY(shadow_table_name, dataset_id)
) ENGINE=InnoDB;

-- ------------------------------------------------------------------
-- 3. Change-capture triggers for shadow_vec_docs
-- ------------------------------------------------------------------

-- Helper procedure to emit a log row. Uses JSON_OBJECT and hex-encodes embeddings.
DELIMITER $$
CREATE PROCEDURE vec_log_change(
    IN p_dataset VARCHAR(191),
    IN p_shadow  VARCHAR(191),
    IN p_scn     BIGINT,
    IN p_op      VARCHAR(10),
    IN p_id      VARCHAR(191),
    IN p_content MEDIUMTEXT,
    IN p_meta    MEDIUMTEXT,
    IN p_embedding LONGBLOB,
    IN p_model   VARCHAR(191),
    IN p_archived TINYINT(1)
)
BEGIN
    INSERT INTO vec_shadow_log(dataset_id, shadow_table, scn, op, document_id, payload)
    VALUES (
        p_dataset,
        p_shadow,
        p_scn,
        p_op,
        p_id,
        JSON_OBJECT(
            'dataset_id', p_dataset,
            'id', p_id,
            'content', p_content,
            'meta', p_meta,
            'embedding', LOWER(HEX(p_embedding)),
            'embedding_model', p_model,
            'scn', p_scn,
            'archived', p_archived
        )
    );
END$$
DELIMITER ;

DELIMITER $$
CREATE TRIGGER shadow_vec_docs_ai AFTER INSERT ON shadow_vec_docs
FOR EACH ROW
BEGIN
    DECLARE v_scn BIGINT;
    INSERT INTO vec_dataset_scn(dataset_id, next_scn)
    VALUES (NEW.dataset_id, NEW.scn + 1)
    ON DUPLICATE KEY UPDATE next_scn = GREATEST(next_scn + 1, NEW.scn);
    UPDATE vec_dataset SET last_scn = NEW.scn WHERE dataset_id = NEW.dataset_id;
    CALL vec_log_change(
        NEW.dataset_id,
        'shadow_vec_docs',
        NEW.scn,
        'insert',
        NEW.id,
        NEW.content,
        NEW.meta,
        NEW.embedding,
        NEW.embedding_model,
        NEW.archived
    );
END$$

CREATE TRIGGER shadow_vec_docs_au AFTER UPDATE ON shadow_vec_docs
FOR EACH ROW
BEGIN
    CALL vec_log_change(
        NEW.dataset_id,
        'shadow_vec_docs',
        NEW.scn,
        'update',
        NEW.id,
        NEW.content,
        NEW.meta,
        NEW.embedding,
        NEW.embedding_model,
        NEW.archived
    );
END$$

CREATE TRIGGER shadow_vec_docs_ad AFTER DELETE ON shadow_vec_docs
FOR EACH ROW
BEGIN
    DECLARE v_scn BIGINT;
    SET v_scn = vec_next_scn(OLD.dataset_id);
    CALL vec_log_change(
        OLD.dataset_id,
        'shadow_vec_docs',
        v_scn,
        'delete',
        OLD.id,
        OLD.content,
        OLD.meta,
        OLD.embedding,
        OLD.embedding_model,
        1
    );
END$$
DELIMITER ;

-- ------------------------------------------------------------------
-- Notes:
-- - The ingestion pipeline should call SELECT vec_next_scn('dataset') to obtain
--   the SCN before inserting/updating a document, then store that SCN in the row.
-- - Triggers persist each mutation to vec_shadow_log so downstream sqlite-vec
--   replicas can replay changes deterministically.
