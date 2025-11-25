package vector

import (
	"context"
	"database/sql"
	"fmt"
)

// SQLiteStore is a minimal implementation of Store that uses a SQLite
// database for durable storage. Vector similarity is not yet implemented;
// SimilaritySearch currently returns the first k documents in insertion
// order. This provides a working baseline that can be extended with a real
// index in later phases.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates a new SQLite-backed Store. It ensures the base docs
// schema exists in the provided database.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("vector: db is nil")
	}
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

// AddDocuments inserts documents into the docs table. For this initial
// implementation, Document.ID must be non-empty; ID generation can be added
// later as needed.
func (s *SQLiteStore) AddDocuments(ctx context.Context, docs []Document) ([]string, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO docs(id, content, meta, embedding) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		if d.ID == "" {
			return nil, fmt.Errorf("vector: Document.ID must be set in AddDocuments for now")
		}
		// Encode embedding (if present) into a BLOB.
		emb, err := EncodeEmbedding(d.Embedding)
		if err != nil {
			return nil, err
		}
		if _, err := stmt.ExecContext(ctx, d.ID, d.Content, d.Metadata, emb); err != nil {
			return nil, err
		}
		ids = append(ids, d.ID)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// SimilaritySearch currently ignores the queryEmbedding and returns up to k
// documents from the docs table in insertion order. This will be replaced by
// a real vector search implementation in later phases.
func (s *SQLiteStore) SimilaritySearch(ctx context.Context, queryEmbedding []float32, k int) ([]Document, error) {
	if k <= 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, content, meta FROM docs ORDER BY rowid LIMIT ?`, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Content, &d.Metadata); err != nil {
			return nil, err
		}
		// Embedding is not loaded in this baseline implementation.
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Remove deletes a document by ID from the docs table. Vector index removal
// will be added once a real index is wired.
func (s *SQLiteStore) Remove(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("vector: Remove called with empty id")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	_, err := s.db.ExecContext(ctx, `DELETE FROM docs WHERE id = ?`, id)
	return err
}

// Ensure SQLiteStore satisfies the Store interface.
var _ Store = (*SQLiteStore)(nil)
