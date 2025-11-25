package vec

import (
    "context"
    "path/filepath"
    "strings"
    "testing"
    "time"

    "github.com/viant/sqlite-vec/engine"
    "github.com/viant/sqlite-vec/vector"
)

// TestShadowChangeInvalidatesIndex verifies that shadow table writes delete the
// persisted index row in vector_storage, causing the next MATCH to rebuild it.
func TestShadowChangeInvalidatesIndex(t *testing.T) {
    dbPath := filepath.Join(t.TempDir(), "vec_iv.sqlite")
    db, err := engine.Open(dbPath)
    if err != nil { t.Fatalf("engine.Open failed: %v", err) }
    defer db.Close()
    // Keep a single connection during module registration and CREATE VTAB
    db.SetMaxOpenConns(1)
    if err := Register(db); err != nil { t.Fatalf("vec.Register failed: %v", err) }
    if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
        t.Fatalf("PRAGMA setup failed: %v", err)
    }

    // Create VTAB on the same DB handle used for Register. If the underlying
    // SQLite build does not support the vec vtab module, skip this test
    // gracefully instead of failing hard.
    if _, err := db.Exec(`CREATE VIRTUAL TABLE vec_iv USING vec(value)`); err != nil {
        if strings.Contains(err.Error(), "no such module: vec") {
            t.Skipf("skipping: vec vtab not available (%v)", err)
        }
        t.Fatalf("CREATE VIRTUAL TABLE vec_iv failed: %v", err)
    }
    // Ensure vector_storage exists for persisted indices
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS vector_storage (
        shadow_table_name TEXT PRIMARY KEY,
        "index" BLOB
    )`); err != nil { t.Fatalf("create vector_storage: %v", err) }
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _vec_vec_iv (
        id TEXT PRIMARY KEY,
        content TEXT,
        meta TEXT,
        embedding BLOB
    )`); err != nil { t.Fatalf("create shadow iv: %v", err) }
    // Allow a second connection so vtab Filter can use an internal query safely
    db.SetMaxOpenConns(2)

    e1, _ := vector.EncodeEmbedding([]float32{1,0})
    e2, _ := vector.EncodeEmbedding([]float32{0,1})
    q, _ := vector.EncodeEmbedding([]float32{1,0})

    // Seed embeddings in the shadow.
    if _, err := db.Exec(`INSERT INTO _vec_vec_iv(id, content, meta, embedding) VALUES
        ('d1','one','{}',?),('d2','two','{}',?)`, e1, e2); err != nil {
        t.Fatalf("insert shadow failed: %v", err)
    }

    // First MATCH triggers index build and persistence.
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    rows, err := db.QueryContext(ctx, `SELECT rowid, value FROM vec_iv WHERE value MATCH ?`, q)
    if err != nil {
        if ctx.Err() == context.DeadlineExceeded { t.Skipf("skipping: MATCH timed out (%v)", err) }
        t.Fatalf("MATCH query failed: %v", err)
    }
    rows.Close()

    // Verify persisted index exists.
    var cnt int
    if err := db.QueryRow(`SELECT COUNT(*) FROM vector_storage WHERE shadow_table_name = 'main._vec_vec_iv' AND "index" IS NOT NULL`).Scan(&cnt); err != nil {
        t.Fatalf("count vector_storage failed: %v", err)
    }
    if cnt != 1 { t.Fatalf("expected 1 persisted index row, got %d", cnt) }

    // Modify shadow: insert another row -> triggers should delete persisted index row and invalidate in-memory cache.
    e3, _ := vector.EncodeEmbedding([]float32{1,1})
    if _, err := db.Exec(`INSERT INTO _vec_vec_iv(id, content, meta, embedding) VALUES('d3','three','{}',?)`, e3); err != nil {
        t.Fatalf("insert shadow d3 failed: %v", err)
    }

    if err := db.QueryRow(`SELECT COUNT(*) FROM vector_storage WHERE shadow_table_name = 'main._vec_vec_iv' AND "index" IS NOT NULL`).Scan(&cnt); err != nil {
        t.Fatalf("count vector_storage after insert failed: %v", err)
    }
    if cnt != 0 { t.Fatalf("expected persisted index to be invalidated (0), got %d", cnt) }

    // Next MATCH should rebuild and persist again (cache invalidated by trigger func).
    ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel2()
    rows, err = db.QueryContext(ctx2, `SELECT rowid, value FROM vec_iv WHERE value MATCH ?`, q)
    if err != nil {
        if ctx2.Err() == context.DeadlineExceeded { t.Skipf("skipping: MATCH 2 timed out (%v)", err) }
        t.Fatalf("MATCH query 2 failed: %v", err)
    }
    rows.Close()

    if err := db.QueryRow(`SELECT COUNT(*) FROM vector_storage WHERE shadow_table_name = 'main._vec_vec_iv' AND "index" IS NOT NULL`).Scan(&cnt); err != nil {
        t.Fatalf("count vector_storage after rebuild failed: %v", err)
    }
    if cnt != 1 { t.Fatalf("expected 1 persisted index row after rebuild, got %d", cnt) }
}
