package vector

import (
	"testing"

	"github.com/viant/sqlite-vec/engine"
)

// TestSQLOrderByVecCosine validates that the vec_cosine SQL function can be
// used in an ORDER BY clause over the docs table, using embeddings stored as
// BLOBs via EncodeEmbedding.
func TestSQLOrderByVecCosine(t *testing.T) {
    // Register functions before any connection work
    if err := engine.RegisterVectorFunctions(nil); err != nil {
        t.Fatalf("RegisterVectorFunctions: %v", err)
    }
    db, err := engine.Open(":memory:")
	if err != nil {
		t.Fatalf("engine.Open(:memory:) failed: %v", err)
	}
	defer db.Close()

    if err := EnsureSchema(db); err != nil {
        t.Fatalf("EnsureSchema failed: %v", err)
    }

	// Two simple embeddings: e1=[1,0], e2=[0,1]; query=[1,0].
	e1, err := EncodeEmbedding([]float32{1, 0})
	if err != nil {
		t.Fatalf("EncodeEmbedding e1 failed: %v", err)
	}
	e2, err := EncodeEmbedding([]float32{0, 1})
	if err != nil {
		t.Fatalf("EncodeEmbedding e2 failed: %v", err)
	}
	q, err := EncodeEmbedding([]float32{1, 0})
	if err != nil {
		t.Fatalf("EncodeEmbedding q failed: %v", err)
	}

	// Insert two docs with these embeddings.
	if _, err := db.Exec(`INSERT INTO docs(id, content, meta, embedding) VALUES
		('d1', 'one', '{}', ?),
		('d2', 'two', '{}', ?)`, e1, e2); err != nil {
		t.Fatalf("insert into docs failed: %v", err)
	}

	// Order by cosine similarity to q in descending order; d1 should be first.
	rows, err := db.Query(`SELECT id FROM docs ORDER BY vec_cosine(embedding, ?) DESC`, q)
	if err != nil {
		t.Fatalf("ORDER BY vec_cosine query failed: %v", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id failed: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %d", len(ids))
	}
	if ids[0] != "d1" || ids[1] != "d2" {
		t.Fatalf("ORDER BY vec_cosine returned ids=%v, want [d1 d2]", ids)
	}
}
