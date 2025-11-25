package vector

import (
	"testing"

	"github.com/viant/sqlite-vec/engine"
)

// TestEnsureSchema verifies that EnsureSchema creates the docs table without
// error on a fresh in-memory database.
func TestEnsureSchema(t *testing.T) {
	db, err := engine.Open(":memory:")
	if err != nil {
		t.Fatalf("engine.Open(:memory:) failed: %v", err)
	}
	defer db.Close()

	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema failed: %v", err)
	}

	// Sanity check: we can insert a row into docs.
	if _, err := db.Exec(`INSERT INTO docs(id, content, meta, embedding) VALUES('1', 'hello', '{}', X'')`); err != nil {
		t.Fatalf("insert into docs failed: %v", err)
	}
}

