package vecsync

import (
	"fmt"
	"strings"
)

const (
	// DefaultLogTable is the upstream change-log table that captures row-level SCN events.
	DefaultLogTable = "vec_shadow_log"

	// DefaultSeqTable stores the next SCN per dataset on the upstream database.
	DefaultSeqTable = "vec_dataset_scn"
)

// ShadowTableDDL returns the base DDL snippet for upstream shadow tables with SCN support.
// Callers can embed this SQL when creating dataset-aware document stores. The DDL assumes
// SQLite/MySQL-compatible syntax; adapt as needed for other engines.
func ShadowTableDDL(table string) string {
	return `CREATE TABLE IF NOT EXISTS ` + table + ` (
    dataset_id TEXT NOT NULL,
    id         TEXT NOT NULL,
    content    TEXT,
    meta       TEXT,
    embedding  BLOB,
    scn        INTEGER NOT NULL DEFAULT 0,
    embedding_model TEXT,
    archived   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(dataset_id, id)
);`
}

// LogTableDDL returns the DDL for vec_shadow_log, which upstream databases populate
// via triggers whenever the shadow table changes.
func LogTableDDL() string {
	return `CREATE TABLE IF NOT EXISTS vec_shadow_log (
    dataset_id   TEXT NOT NULL,
    shadow_table TEXT NOT NULL,
    scn          INTEGER NOT NULL,
    op           TEXT NOT NULL,
    document_id  TEXT NOT NULL,
    payload      BLOB NOT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY(dataset_id, shadow_table, scn)
);`
}

// SeqTableDDL returns the DDL for tracking next SCN per dataset (optional helper).
func SeqTableDDL() string {
	return `CREATE TABLE IF NOT EXISTS vec_dataset_scn (
    dataset_id TEXT PRIMARY KEY,
    next_scn   INTEGER NOT NULL
);`
}

// SQLiteShadowLogTriggers returns the trigger DDL statements required to capture
// inserts, updates, and deletes against a shadow table into vec_shadow_log using
// SQLite syntax. The payload is serialized as JSON with a hex-encoded embedding.
func SQLiteShadowLogTriggers(shadowTable, seqTable, logTable string) []string {
	if seqTable == "" {
		seqTable = DefaultSeqTable
	}
	if logTable == "" {
		logTable = DefaultLogTable
	}
	base := sanitizeIdentifier(shadowTable)
	payload := func(alias string) string {
		return fmt.Sprintf(`json_object(
        'dataset_id', %[1]s.dataset_id,
        'id', %[1]s.id,
        'content', %[1]s.content,
        'meta', %[1]s.meta,
        'embedding', lower(hex(%[1]s.embedding)),
        'embedding_model', %[1]s.embedding_model,
        'scn', %[1]s.scn,
        'archived', %[1]s.archived
    )`, alias)
	}
	advance := func(alias string) string {
		return fmt.Sprintf(`INSERT INTO %[1]s(dataset_id, next_scn)
    VALUES (%[2]s.dataset_id, 1)
    ON CONFLICT(dataset_id) DO UPDATE SET next_scn = next_scn + 1;`, seqTable, alias)
	}
	scnExpr := func(alias string) string {
		return fmt.Sprintf(`(SELECT next_scn FROM %s WHERE dataset_id = %s.dataset_id)`, seqTable, alias)
	}

	insertTrig := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ai AFTER INSERT ON %s
BEGIN
    %s
    INSERT INTO %s(dataset_id, shadow_table, scn, op, document_id, payload)
    VALUES (
        NEW.dataset_id,
        '%s',
        %s,
        'insert',
        NEW.id,
        %s
    );
END;`, base, shadowTable, advance("NEW"), logTable, shadowTable, scnExpr("NEW"), payload("NEW"))

	updateTrig := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_au AFTER UPDATE ON %s
BEGIN
    %s
    INSERT INTO %s(dataset_id, shadow_table, scn, op, document_id, payload)
    VALUES (
        NEW.dataset_id,
        '%s',
        %s,
        'update',
        NEW.id,
        %s
    );
END;`, base, shadowTable, advance("NEW"), logTable, shadowTable, scnExpr("NEW"), payload("NEW"))

	deleteTrig := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ad AFTER DELETE ON %s
BEGIN
    %s
    INSERT INTO %s(dataset_id, shadow_table, scn, op, document_id, payload)
    VALUES (
        OLD.dataset_id,
        '%s',
        %s,
        'delete',
        OLD.id,
        %s
    );
END;`, base, shadowTable, advance("OLD"), logTable, shadowTable, scnExpr("OLD"), payload("OLD"))

	return []string{insertTrig, updateTrig, deleteTrig}
}

// MySQLShadowLogTriggers returns AFTER INSERT/UPDATE/DELETE trigger definitions
// that populate vec_shadow_log with JSON payloads (hex-encoded embeddings) and
// per-dataset SCNs. Callers are responsible for wrapping the statements with an
// appropriate DELIMITER when executing them.
func MySQLShadowLogTriggers(shadowTable, seqTable, logTable string) []string {
	if seqTable == "" {
		seqTable = DefaultSeqTable
	}
	if logTable == "" {
		logTable = DefaultLogTable
	}
	base := sanitizeIdentifier(shadowTable)
	payload := func(alias string) string {
		return fmt.Sprintf(`JSON_OBJECT(
        'dataset_id', %[1]s.dataset_id,
        'id', %[1]s.id,
        'content', %[1]s.content,
        'meta', %[1]s.meta,
        'embedding', LOWER(HEX(%[1]s.embedding)),
        'embedding_model', %[1]s.embedding_model,
        'scn', %[1]s.scn,
        'archived', %[1]s.archived
    )`, alias)
	}
	advance := func(alias string) string {
		return fmt.Sprintf(`    INSERT INTO %[1]s(dataset_id, next_scn)
    VALUES (%[2]s.dataset_id, 1)
    ON DUPLICATE KEY UPDATE next_scn = next_scn + 1;
    SELECT next_scn INTO @vecsync_scn FROM %[1]s WHERE dataset_id = %[2]s.dataset_id;`, seqTable, alias)
	}

	insertTrig := fmt.Sprintf(`CREATE TRIGGER %s_ai AFTER INSERT ON %s
FOR EACH ROW
BEGIN
%s
    INSERT INTO %s(dataset_id, shadow_table, scn, op, document_id, payload)
    VALUES (
        NEW.dataset_id,
        '%s',
        @vecsync_scn,
        'insert',
        NEW.id,
        %s
    );
END;`, base, shadowTable, advance("NEW"), logTable, shadowTable, payload("NEW"))

	updateTrig := fmt.Sprintf(`CREATE TRIGGER %s_au AFTER UPDATE ON %s
FOR EACH ROW
BEGIN
%s
    INSERT INTO %s(dataset_id, shadow_table, scn, op, document_id, payload)
    VALUES (
        NEW.dataset_id,
        '%s',
        @vecsync_scn,
        'update',
        NEW.id,
        %s
    );
END;`, base, shadowTable, advance("NEW"), logTable, shadowTable, payload("NEW"))

	deleteTrig := fmt.Sprintf(`CREATE TRIGGER %s_ad AFTER DELETE ON %s
FOR EACH ROW
BEGIN
%s
    INSERT INTO %s(dataset_id, shadow_table, scn, op, document_id, payload)
    VALUES (
        OLD.dataset_id,
        '%s',
        @vecsync_scn,
        'delete',
        OLD.id,
        %s
    );
END;`, base, shadowTable, advance("OLD"), logTable, shadowTable, payload("OLD"))

	return []string{insertTrig, updateTrig, deleteTrig}
}

func sanitizeIdentifier(name string) string {
	if name == "" {
		return ""
	}
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return replacer.Replace(name)
}
