package vector

import (
	"context"
	"testing"

	"github.com/viant/sqlite-vec/engine"
)

// TestSQLiteStore_AddSearchRemove exercises the minimal SQLiteStore
// implementation: inserting documents, performing a basic search (currently
// order by rowid), and removing a document.
func TestSQLiteStore_AddSearchRemove(t *testing.T) {
	db, err := engine.Open(":memory:")
	if err != nil {
		t.Fatalf("engine.Open(:memory:) failed: %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}

	docs := []Document{
		{ID: "d1", Content: "first", Metadata: "{}"},
		{ID: "d2", Content: "second", Metadata: "{}"},
		{ID: "d3", Content: "third", Metadata: "{}"},
	}

	ids, err := store.AddDocuments(context.Background(), docs)
	if err != nil {
		t.Fatalf("AddDocuments failed: %v", err)
	}
	if len(ids) != len(docs) {
		t.Fatalf("AddDocuments returned %d ids, want %d", len(ids), len(docs))
	}

	// Basic search: currently returns first k docs in insertion order.
	out, err := store.SimilaritySearch(context.Background(), nil, 2)
	if err != nil {
		t.Fatalf("SimilaritySearch failed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("SimilaritySearch returned %d docs, want 2", len(out))
	}
	if out[0].ID != "d1" || out[1].ID != "d2" {
		t.Errorf("SimilaritySearch order = [%s, %s], want [d1, d2]", out[0].ID, out[1].ID)
	}

	// Remove a document and ensure it no longer appears in results.
	if err := store.Remove(context.Background(), "d2"); err != nil {
		t.Fatalf("Remove(d2) failed: %v", err)
	}
	out, err = store.SimilaritySearch(context.Background(), nil, 10)
	if err != nil {
		t.Fatalf("SimilaritySearch after remove failed: %v", err)
	}
	for _, d := range out {
		if d.ID == "d2" {
			t.Fatalf("expected d2 to be removed, but found in results")
		}
	}
}

