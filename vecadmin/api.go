package vecadmin

import (
    "context"
    "database/sql"
    "fmt"
    "strings"

    "github.com/viant/sqlite-vec/index/bruteforce"
    "github.com/viant/sqlite-vec/vector"
    "modernc.org/sqlite/vtab"
)

// Module provides administrative operations via a virtual table.
// Usage:
//   CREATE VIRTUAL TABLE vec_admin USING vec_admin(op);
//   SELECT op FROM vec_admin WHERE op MATCH 'main._vec_vec_knn'; -- rebuild index
// Returns a single row with op='reindexed:<count>' on success.
type Module struct { db *sql.DB }

type Table struct { db *sql.DB }

type Cursor struct {
    table *Table
    rows  []string
    pos   int
}

func Register(db *sql.DB) error {
    if err := vtab.RegisterModule(db, "vec_admin", &Module{db: db}); err != nil {
        if !strings.Contains(err.Error(), "already registered") { return err }
    }
    return nil
}

func (m *Module) Create(ctx vtab.Context, args []string) (vtab.Table, error) {
    if len(args) < 3 { return nil, fmt.Errorf("vec_admin: need at least 3 args") }
    // Single TEXT column `op` reporting results.
    if err := ctx.Declare(fmt.Sprintf("CREATE TABLE %s(op)", args[2])); err != nil { return nil, err }
    return &Table{db: m.db}, nil
}
func (m *Module) Connect(ctx vtab.Context, args []string) (vtab.Table, error) {
    if len(args) < 3 { return nil, fmt.Errorf("vec_admin: need at least 3 args") }
    if err := ctx.Declare(fmt.Sprintf("CREATE TABLE %s(op)", args[2])); err != nil { return nil, err }
    return &Table{db: m.db}, nil
}

func (t *Table) BestIndex(info *vtab.IndexInfo) error {
    for i := range info.Constraints {
        c := &info.Constraints[i]
        if !c.Usable { continue }
        if c.Column == 0 && c.Op == vtab.OpMATCH { c.ArgIndex = 1; info.IdxNum = 1; break }
    }
    return nil
}

func (t *Table) Open() (vtab.Cursor, error) { return &Cursor{table: t}, nil }
func (t *Table) Disconnect() error { return nil }
func (t *Table) Destroy() error { return nil }

func (c *Cursor) Filter(idxNum int, idxStr string, vals []vtab.Value) error {
    c.rows = nil
    c.pos = 0
    if idxNum != 1 || len(vals) == 0 || vals[0] == nil { return nil }
    shadow, ok := vals[0].(string)
    if !ok { return fmt.Errorf("vec_admin: MATCH expects shadow table name as TEXT") }
    n, err := reindex(c.table.db, shadow)
    if err != nil { return err }
    c.rows = []string{fmt.Sprintf("reindexed:%d", n)}
    return nil
}

func (c *Cursor) Next() error { if c.pos < len(c.rows) { c.pos++ }; return nil }
func (c *Cursor) Eof() bool { return c.pos >= len(c.rows) }
func (c *Cursor) Column(col int) (vtab.Value, error) {
    if c.pos < 0 || c.pos >= len(c.rows) { return nil, fmt.Errorf("vec_admin: Column out of range") }
    if col == 0 { return c.rows[c.pos], nil }
    return nil, nil
}
func (c *Cursor) Rowid() (int64, error) { return int64(c.pos + 1), nil }
func (c *Cursor) Close() error { c.rows = nil; c.pos = 0; return nil }

// reindex rebuilds and persists a bruteforce index for the given shadow table name.
func reindex(db *sql.DB, shadow string) (int, error) {
    ctx := context.Background()
    // Acquire single-writer gate for this reindex via transaction. BEGIN IMMEDIATE
    // ensures a write reservation and cooperates with busy_timeout.
    if _, err := db.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
        return 0, err
    }
    defer func() { _, _ = db.ExecContext(ctx, `ROLLBACK`) }()

    // Load ids and embeddings
    q := fmt.Sprintf("SELECT id, embedding FROM %s WHERE embedding IS NOT NULL", shadow)
    rows, err := db.QueryContext(ctx, q)
    if err != nil { return 0, err }
    defer rows.Close()
    var ids []string
    var vecs [][]float32
    for rows.Next() {
        var id string
        var emb []byte
        if err := rows.Scan(&id, &emb); err != nil { return 0, err }
        if len(emb) == 0 { continue }
        v, err := vector.DecodeEmbedding(emb)
        if err != nil { return 0, err }
        ids = append(ids, id)
        vecs = append(vecs, v)
    }
    if err := rows.Err(); err != nil { return 0, err }
    idx := &bruteforce.Index{}
    if err := idx.Build(ids, vecs); err != nil { return 0, err }
    data, err := idx.MarshalBinary()
    if err != nil { return 0, err }
    if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO vector_storage(shadow_table_name, "index") VALUES(?, ?)`, shadow, data); err != nil {
        return 0, err
    }
    if _, err := db.ExecContext(ctx, `COMMIT`); err != nil {
        return 0, err
    }
    return len(ids), nil
}
