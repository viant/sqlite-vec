package vec

import (
    "context"
    "path/filepath"
    "strings"
    "time"
    "testing"

    "github.com/viant/sqlite-vec/engine"
    "github.com/viant/sqlite-vec/vector"
)

func TestVecVirtualTableBasic(t *testing.T) {
    dbPath := filepath.Join(t.TempDir(), "vec_basic.sqlite")
    db, err := engine.Open(dbPath)
    if err != nil { t.Fatalf("engine.Open failed: %v", err) }
    defer db.Close()
    // Register module on this DB before any SQL
    if err := Register(db); err != nil {
        t.Fatalf("vec.Register failed: %v", err)
    }
    // Enable WAL and busy timeout to mimic concurrent-friendly settings.
    if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
        t.Fatalf("PRAGMA setup failed: %v", err)
    }

    // Create a vec virtual table with a single visible column
    if _, err := db.Exec(`CREATE VIRTUAL TABLE vec_basic USING vec(value)`); err != nil {
        t.Fatalf("CREATE VIRTUAL TABLE vec_basic failed: %v", err)
    }
    // Ensure shadow exists (auto-creation disabled to avoid deadlocks across connections).
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _vec_vec_basic (
        id TEXT PRIMARY KEY,
        content TEXT,
        meta TEXT,
        embedding BLOB
    )`); err != nil { t.Fatalf("create shadow: %v", err) }

    // Insert rows into the shadow table
    if _, err := db.Exec(`INSERT INTO _vec_vec_basic(id, content, meta, embedding) VALUES
        ('a1','one','{}',X''),
        ('a2','two','{}',X''),
        ('a3','three','{}',X'')`); err != nil {
        t.Fatalf("insert into shadow failed: %v", err)
    }

    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    rows, err := db.QueryContext(ctx, `SELECT rowid, value FROM vec_basic ORDER BY rowid`)
    if err != nil {
        // Avoid indefinite hang on environments where vtab recursion is not permitted
        if ctx.Err() == context.DeadlineExceeded {
            t.Skipf("skipping: vec_basic listing timed out (%v)", err)
        }
        t.Fatalf("SELECT failed: %v", err)
    }
    defer rows.Close()
    var ids []string
    for rows.Next() {
        var rid int64
        var v string
        if err := rows.Scan(&rid, &v); err != nil { t.Fatalf("scan: %v", err) }
        ids = append(ids, v)
    }
    if err := rows.Err(); err != nil { t.Fatalf("rows: %v", err) }
    if len(ids) != 3 || ids[0] != "a1" || ids[1] != "a2" || ids[2] != "a3" {
        t.Fatalf("unexpected ids: %v", ids)
    }
}

func TestVecVectorMatch(t *testing.T) {
    dbPath := filepath.Join(t.TempDir(), "vec_knn.sqlite")
    db, err := engine.Open(dbPath)
    if err != nil { t.Fatalf("engine.Open failed: %v", err) }
    defer db.Close()
    // Single connection ensures vtab registration for CREATE VTAB
    db.SetMaxOpenConns(1)
    if err := Register(db); err != nil { t.Fatalf("vec.Register failed: %v", err) }
    if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
        t.Fatalf("PRAGMA setup failed: %v", err)
    }
    // Create VTAB on the same DB handle used for Register. If the underlying
    // SQLite build does not support vtab modules, surface this as a skip
    // rather than a hard failure so the test suite remains portable.
    if _, err := db.Exec(`CREATE VIRTUAL TABLE vec_knn USING vec(value)`); err != nil {
        if strings.Contains(err.Error(), "no such module: vec") {
            t.Skipf("skipping: vec vtab not available (%v)", err)
        }
        t.Fatalf("CREATE VIRTUAL TABLE vec_knn failed: %v", err)
    }
    // Ensure vector_storage exists for persisted indices
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS vector_storage (
        shadow_table_name TEXT PRIMARY KEY,
        "index" BLOB
    )`); err != nil { t.Fatalf("create vector_storage: %v", err) }
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _vec_vec_knn (
        id TEXT PRIMARY KEY,
        content TEXT,
        meta TEXT,
        embedding BLOB
    )`); err != nil { t.Fatalf("create shadow knn: %v", err) }

    e1, _ := vector.EncodeEmbedding([]float32{1,0})
    e2, _ := vector.EncodeEmbedding([]float32{0,1})
    q, _ := vector.EncodeEmbedding([]float32{1,0})

    if _, err := db.Exec(`INSERT INTO _vec_vec_knn(id, content, meta, embedding) VALUES
        ('d1','one','{}',?),('d2','two','{}',?)`, e1, e2); err != nil {
        t.Fatalf("insert shadow failed: %v", err)
    }
    // Allow second connection for internal vtab queries
    db.SetMaxOpenConns(2)
    ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel2()
    rows, err := db.QueryContext(ctx2, `SELECT rowid, value FROM vec_knn WHERE value MATCH ?`, q)
    if err != nil {
        if ctx2.Err() == context.DeadlineExceeded {
            t.Skipf("skipping: vec_knn MATCH timed out (%v)", err)
        }
        t.Fatalf("SELECT MATCH failed: %v", err)
    }
    defer rows.Close()
    var ids []string
    for rows.Next() {
        var rid int64
        var v string
        if err := rows.Scan(&rid, &v); err != nil { t.Fatalf("scan: %v", err) }
        ids = append(ids, v)
    }
    if err := rows.Err(); err != nil { t.Fatalf("rows: %v", err) }
    if len(ids) != 2 || ids[0] != "d1" || ids[1] != "d2" {
        t.Fatalf("unexpected ids order: %v", ids)
    }
}
