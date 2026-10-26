package vec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Match identifies a document and its similarity score.
type Match struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// Search builds or reads a shared in-memory index without database writes.
// Unlike SQL MATCH callbacks, this entry point preserves caller cancellation.
// Writers must invalidate the dataset after changing its shadow rows.
func Search(ctx context.Context, db *sql.DB, table, dataset string, query []float32) ([]Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db == nil || strings.TrimSpace(table) == "" {
		return nil, fmt.Errorf("vec: database and table are required")
	}
	for _, c := range table {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return nil, fmt.Errorf("vec: invalid table identifier")
		}
	}
	t := &Table{db: db, dbName: "main", tableName: table, shadow: "main._vec_" + table, readOnly: true}
	idx, err := t.ensureIndex(ctx, dataset)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids, scores, err := idx.Query(query, 0)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]Match, len(ids))
	for i, id := range ids {
		result[i] = Match{ID: id, Score: scores[i]}
	}
	return result, nil
}
