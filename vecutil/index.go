package vecutil

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/viant/sqlite-vec/vector"
)

// Index provides a higher-level, Pinecone-style API on top of a vec virtual
// table and its shadow table. It remains embedding-agnostic by requiring an
// EmbedFunc supplied by the caller.
type Index struct {
	DB          *sql.DB
	VirtualName string
	ShadowName  string
	DatasetID   string
	Embed       EmbedFunc
}

// NewIndex constructs an Index for a given vec virtual table name.
//
// The shadow table name is derived using ShadowTableName. The caller is
// responsible for having created both the virtual table and its shadow table
// schema (see README examples).
func NewIndex(db *sql.DB, virtualTable string, datasetID string, embed EmbedFunc) (*Index, error) {
	if db == nil {
		return nil, fmt.Errorf("vecutil: db is nil")
	}
	if embed == nil {
		return nil, fmt.Errorf("vecutil: EmbedFunc is nil")
	}
	return &Index{
		DB:          db,
		VirtualName: virtualTable,
		ShadowName:  ShadowTableName(virtualTable),
		DatasetID:   datasetID,
		Embed:       embed,
	}, nil
}

// Document represents a logical document stored in the vec shadow table.
// Metadata is modeled as a raw JSON (or other encoding) string for maximum
// flexibility.
type Document struct {
	ID      string
	Content string
	Meta    string
}

// Match represents a single similarity search hit.
type Match struct {
	ID      string
	Score   float64
	Content string
	Meta    string
}

// UpsertDocumentsText upserts the provided documents into the shadow table,
// computing embeddings from Content using the Index's EmbedFunc.
//
// This is analogous to client-side text upsert flows in hosted vector
// databases: callers supply free-form text and optional metadata; the index
// handles embedding and storage.
func (ix *Index) UpsertDocumentsText(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}
	for _, d := range docs {
		if err := UpsertShadowDocument(ctx, ix.DB, ix.ShadowName, ix.Embed, ix.DatasetID, d.ID, d.Content, d.Meta); err != nil {
			return err
		}
	}
	return nil
}

// DeleteDocuments removes documents with the given ids from the shadow table.
// Triggers installed by the vec module will invalidate any persisted index
// entries, causing them to be rebuilt on the next MATCH query.
func (ix *Index) DeleteDocuments(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if ix.DB == nil {
		return fmt.Errorf("vecutil: DB is nil on Index")
	}
	stmt := fmt.Sprintf("DELETE FROM %s WHERE dataset_id = ? AND id = ?", ix.ShadowName)
	for _, id := range ids {
		if _, err := ix.DB.ExecContext(ctx, stmt, ix.DatasetID, id); err != nil {
			return err
		}
	}
	return nil
}

// QueryText performs a similarity search using the provided query text. It
// uses the vec virtual table for kNN ordering and then computes cosine
// similarity scores in Go for each match.
//
// When k <= 0, all matches returned by the underlying index are included.
func (ix *Index) QueryText(ctx context.Context, query string, k int) ([]Match, error) {
	if ix.DB == nil {
		return nil, fmt.Errorf("vecutil: DB is nil on Index")
	}
	if ix.Embed == nil {
		return nil, fmt.Errorf("vecutil: EmbedFunc is nil on Index")
	}

	// 1. Embed the query text.
	qVec, err := ix.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	qBlob, err := vector.EncodeEmbedding(qVec)
	if err != nil {
		return nil, err
	}

	// 2. Use the vec virtual table to obtain ordered ids.
	base := fmt.Sprintf("SELECT doc_id FROM %s WHERE dataset_id = ? AND doc_id MATCH ?", ix.VirtualName)
	var rows *sql.Rows
	if k > 0 {
		q := base + " LIMIT ?"
		rows, err = ix.DB.QueryContext(ctx, q, ix.DatasetID, qBlob, k)
	} else {
		rows, err = ix.DB.QueryContext(ctx, base, ix.DatasetID, qBlob)
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
	if len(ids) == 0 {
		return nil, nil
	}

	// 3. For each id, load content/meta/embedding and compute cosine similarity
	//    in Go. This keeps provider logic in the embedding layer while still
	//    exposing a score similar to hosted vector DBs.
	stmt := fmt.Sprintf("SELECT content, meta, embedding FROM %s WHERE dataset_id = ? AND id = ?", ix.ShadowName)
	out := make([]Match, 0, len(ids))
	for _, id := range ids {
		row := ix.DB.QueryRowContext(ctx, stmt, ix.DatasetID, id)
		var content, meta string
		var embBlob []byte
		if err := row.Scan(&content, &meta, &embBlob); err != nil {
			return nil, err
		}
		embVec, err := vector.DecodeEmbedding(embBlob)
		if err != nil {
			return nil, err
		}
		score, err := vector.CosineSimilarity(qVec, embVec)
		if err != nil {
			return nil, err
		}
		out = append(out, Match{
			ID:      id,
			Score:   score,
			Content: content,
			Meta:    meta,
		})
	}
	return out, nil
}
