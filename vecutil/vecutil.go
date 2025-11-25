package vecutil

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/viant/sqlite-vec/vector"
)

// EmbedFunc converts free-form text into an embedding.
//
// Implementations can call any embedding provider (OpenAI, local model,
// other cloud APIs, etc.) as long as they return a slice of float32 values.
// The core sqlite-vec packages remain embedding-agnostic and only depend on
// the numeric vectors and their encoded BLOB representation.
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

// ShadowTableName derives the default shadow table name for a given vec
// virtual table. It mirrors the naming convention used by the vec module,
// which prefixes the table name with _vec_.
//
// For example:
//   ShadowTableName("vec_basic") == "_vec_vec_basic".
//
// Schema/database qualification (e.g. main.) is handled by SQLite; this helper
// only returns the bare table name.
func ShadowTableName(virtualTable string) string {
	return "_vec_" + virtualTable
}

// UpsertShadowDocument inserts or updates a document row in a vec shadow
// table, computing the embedding from content using the provided EmbedFunc.
//
// The shadowTable is typically derived via ShadowTableName or known
// explicitly. It is assumed to follow the schema used by sqlite-vec tests:
//   id        TEXT PRIMARY KEY
//   content   TEXT
//   meta      TEXT
//   embedding BLOB
//
// Table and column names are interpolated into SQL; callers should ensure
// that shadowTable is trusted and not derived from untrusted input.
func UpsertShadowDocument(
	ctx context.Context,
	db *sql.DB,
	shadowTable string,
	embed EmbedFunc,
	id, content, meta string,
) error {
	if db == nil {
		return fmt.Errorf("vecutil: db is nil")
	}
	if embed == nil {
		return fmt.Errorf("vecutil: EmbedFunc is nil")
	}

	vec, err := embed(ctx, content)
	if err != nil {
		return err
	}
	blob, err := vector.EncodeEmbedding(vec)
	if err != nil {
		return err
	}

	stmt := fmt.Sprintf(`
INSERT INTO %s(id, content, meta, embedding)
VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  content = excluded.content,
  meta = excluded.meta,
  embedding = excluded.embedding`, shadowTable)

	_, err = db.ExecContext(ctx, stmt, id, content, meta, blob)
	return err
}

// UpsertVirtualTableDocument is a convenience wrapper around
// UpsertShadowDocument that derives the shadow table name from the virtual
// table name using ShadowTableName.
func UpsertVirtualTableDocument(
	ctx context.Context,
	db *sql.DB,
	virtualTable string,
	embed EmbedFunc,
	id, content, meta string,
) error {
	shadow := ShadowTableName(virtualTable)
	return UpsertShadowDocument(ctx, db, shadow, embed, id, content, meta)
}

// MatchText executes a MATCH query against a vec virtual table by first
// converting the free-form query text into an embedding via EmbedFunc.
//
// It returns the list of ids (the visible column of the virtual table) in the
// order returned by the underlying index. When limit <= 0, all matches are
// returned; otherwise a SQL LIMIT is applied.
func MatchText(
	ctx context.Context,
	db *sql.DB,
	virtualTable string,
	embed EmbedFunc,
	query string,
	limit int,
) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("vecutil: db is nil")
	}
	if embed == nil {
		return nil, fmt.Errorf("vecutil: EmbedFunc is nil")
	}

	vec, err := embed(ctx, query)
	if err != nil {
		return nil, err
	}
	blob, err := vector.EncodeEmbedding(vec)
	if err != nil {
		return nil, err
	}

	base := fmt.Sprintf("SELECT value FROM %s WHERE value MATCH ?", virtualTable)
	var rows *sql.Rows
	if limit > 0 {
		q := base + " LIMIT ?"
		rows, err = db.QueryContext(ctx, q, blob, limit)
	} else {
		rows, err = db.QueryContext(ctx, base, blob)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

