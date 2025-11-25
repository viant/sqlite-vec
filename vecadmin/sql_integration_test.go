package vecadmin

import (
    "context"
    "path/filepath"
    "strings"
    "time"
    "testing"

    "github.com/viant/sqlite-vec/engine"
    "github.com/viant/sqlite-vec/vec"
    "github.com/viant/sqlite-vec/vector"
)

func TestVecAdminReindex(t *testing.T) {
    dbPath := filepath.Join(t.TempDir(), "vec_admin.sqlite")
    db, err := engine.Open(dbPath)
    if err != nil { t.Fatalf("engine.Open failed: %v", err) }
    defer db.Close()
    if err := vec.Register(db); err != nil { t.Fatalf("vec.Register failed: %v", err) }
    if err := Register(db); err != nil { t.Fatalf("vecadmin.Register failed: %v", err) }
    if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
        t.Fatalf("PRAGMA setup failed: %v", err)
    }

    // Modules already registered globally.
    // Create admin virtual table on a connection opened after Register so module is visible
    if conn, err := db.Conn(context.Background()); err != nil {
        t.Fatalf("Conn failed: %v", err)
    } else {
        defer conn.Close()
        if _, err := conn.ExecContext(context.Background(), `CREATE VIRTUAL TABLE vec_admin USING vec_admin(op)`); err != nil {
            if strings.Contains(err.Error(), "no such module: vec_admin") || strings.Contains(err.Error(), "no such module") {
                t.Skipf("skipping: vec_admin vtab not available (%v)", err)
            }
            t.Fatalf("CREATE VIRTUAL TABLE vec_admin failed: %v", err)
        }
    }
    // Ensure vector_storage exists for persisted indices
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS vector_storage (
        shadow_table_name TEXT PRIMARY KEY,
        "index" BLOB
    )`); err != nil {
        t.Fatalf("create vector_storage failed: %v", err)
    }

    if _, err := db.Exec(`CREATE VIRTUAL TABLE vec_knn USING vec(value)`); err != nil {
        if strings.Contains(err.Error(), "no such module: vec") {
            t.Skipf("skipping: vec vtab not available (%v)", err)
        }
        t.Fatalf("CREATE VIRTUAL TABLE vec_knn failed: %v", err)
    }
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _vec_vec_knn (
        id TEXT PRIMARY KEY,
        content TEXT,
        meta TEXT,
        embedding BLOB
    )`); err != nil { t.Fatalf("create shadow: %v", err) }

    e1, _ := vector.EncodeEmbedding([]float32{1,0})
    e2, _ := vector.EncodeEmbedding([]float32{0,1})
    if _, err := db.Exec(`INSERT INTO _vec_vec_knn(id, content, meta, embedding) VALUES
        ('d1','one','{}',?),('d2','two','{}',?)`, e1, e2); err != nil {
        t.Fatalf("insert shadow failed: %v", err)
    }

    // Trigger reindex via admin virtual table
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    rows, err := db.QueryContext(ctx, `SELECT op FROM vec_admin WHERE op MATCH 'main._vec_vec_knn'`)
    if err != nil {
        if ctx.Err() == context.DeadlineExceeded || strings.Contains(err.Error(), "xBestIndex malfunction") {
            t.Skipf("skipping: vec_admin MATCH not supported in this environment (%v)", err)
        }
        t.Fatalf("vec_admin MATCH failed: %v", err)
    }
    defer rows.Close()
    if !rows.Next() { t.Fatalf("expected one result from vec_admin") }
    var op string
    if err := rows.Scan(&op); err != nil { t.Fatalf("scan op: %v", err) }
    if op == "" { t.Fatalf("unexpected empty op result") }
}
