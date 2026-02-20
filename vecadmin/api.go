package vecadmin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/viant/sqlite-vec/engine"
	"github.com/viant/sqlite-vec/index/bruteforce"
	"github.com/viant/sqlite-vec/vector"
	"modernc.org/sqlite/vtab"
)

// Module provides administrative operations via a virtual table.
// Usage:
//
//	CREATE VIRTUAL TABLE vec_admin USING vec_admin(op);
//	SELECT op FROM vec_admin WHERE op MATCH 'main._vec_vec_knn:rootA'; -- rebuild dataset slice
//
// Returns a single row with op='reindexed:<count>' on success.
type Module struct{ db *sql.DB }

type Table struct {
	db    *sql.DB
	ownDB bool
}

type Cursor struct {
	table *Table
	rows  []string
	pos   int
}

func Register(db *sql.DB) error {
	if err := vtab.RegisterModule(db, "vec_admin", &Module{db: db}); err != nil {
		if !strings.Contains(err.Error(), "already registered") {
			return err
		}
	}
	return nil
}

func (m *Module) Create(ctx vtab.Context, args []string) (vtab.Table, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("vec_admin: need at least 3 args")
	}
	if err := ctx.EnableConstraintSupport(); err != nil {
		return nil, err
	}
	// Single TEXT column `op` reporting results.
	if err := ctx.Declare(fmt.Sprintf("CREATE TABLE %s(op)", args[2])); err != nil {
		return nil, err
	}
	db, own, err := resolveAdminDB(args[3:])
	if err != nil {
		return nil, err
	}
	if db == nil {
		db = m.db
	}
	return &Table{db: db, ownDB: own}, nil
}
func (m *Module) Connect(ctx vtab.Context, args []string) (vtab.Table, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("vec_admin: need at least 3 args")
	}
	if err := ctx.EnableConstraintSupport(); err != nil {
		return nil, err
	}
	if err := ctx.Declare(fmt.Sprintf("CREATE TABLE %s(op)", args[2])); err != nil {
		return nil, err
	}
	db, own, err := resolveAdminDB(args[3:])
	if err != nil {
		return nil, err
	}
	if db == nil {
		db = m.db
	}
	return &Table{db: db, ownDB: own}, nil
}

func (t *Table) BestIndex(info *vtab.IndexInfo) error {
	for i := range info.Constraints {
		c := &info.Constraints[i]
		if !c.Usable {
			continue
		}
		if c.Column == 0 && c.Op == vtab.OpMATCH {
			c.ArgIndex = 1
			info.IdxNum = 1
			break
		}
	}
	return nil
}

func (t *Table) Open() (vtab.Cursor, error) { return &Cursor{table: t}, nil }
func (t *Table) Disconnect() error {
	if t.ownDB && t.db != nil {
		_ = t.db.Close()
		t.db = nil
	}
	return nil
}
func (t *Table) Destroy() error {
	if t.ownDB && t.db != nil {
		_ = t.db.Close()
		t.db = nil
	}
	return nil
}

func resolveAdminDB(args []string) (*sql.DB, bool, error) {
	dbPath := ""
	for _, raw := range args {
		a := strings.TrimSpace(raw)
		la := strings.ToLower(a)
		if strings.HasPrefix(la, "dbpath=") {
			dbPath = strings.TrimSpace(a[len("dbpath="):])
			break
		}
	}
	if dbPath == "" {
		return nil, false, nil
	}
	db, err := engine.Open(dbPath)
	if err != nil {
		return nil, false, err
	}
	db.SetMaxOpenConns(1)
	return db, true, nil
}

func (c *Cursor) Filter(idxNum int, idxStr string, vals []vtab.Value) error {
	c.rows = nil
	c.pos = 0
	if idxNum != 1 || len(vals) == 0 || vals[0] == nil {
		return nil
	}
	raw, ok := vals[0].(string)
	if !ok {
		return fmt.Errorf("vec_admin: MATCH expects shadow table name as TEXT")
	}
	shadow, dataset, err := parseTarget(raw)
	if err != nil {
		return err
	}
	n, err := reindex(c.table.db, shadow, dataset)
	if err != nil {
		return err
	}
	c.rows = []string{fmt.Sprintf("reindexed:%d", n)}
	return nil
}

func (c *Cursor) Next() error {
	if c.pos < len(c.rows) {
		c.pos++
	}
	return nil
}
func (c *Cursor) Eof() bool { return c.pos >= len(c.rows) }
func (c *Cursor) Column(col int) (vtab.Value, error) {
	if c.pos < 0 || c.pos >= len(c.rows) {
		return nil, fmt.Errorf("vec_admin: Column out of range")
	}
	if col == 0 {
		return c.rows[c.pos], nil
	}
	return nil, nil
}
func (c *Cursor) Rowid() (int64, error) { return int64(c.pos + 1), nil }
func (c *Cursor) Close() error          { c.rows = nil; c.pos = 0; return nil }

// reindex rebuilds and persists a bruteforce index for the given shadow table name.
func reindex(db *sql.DB, shadow, dataset string) (int, error) {
	ctx := context.Background()
	// Acquire single-writer gate for this reindex via transaction. BEGIN IMMEDIATE
	// ensures a write reservation and cooperates with busy_timeout.
	if _, err := db.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	defer func() { _, _ = db.ExecContext(ctx, `ROLLBACK`) }()

	// Load ids and embeddings
	q := fmt.Sprintf("SELECT id, embedding FROM %s WHERE dataset_id = ? AND embedding IS NOT NULL", shadow)
	rows, err := db.QueryContext(ctx, q, dataset)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []string
	var vecs [][]float32
	for rows.Next() {
		var id string
		var emb []byte
		if err := rows.Scan(&id, &emb); err != nil {
			return 0, err
		}
		if len(emb) == 0 {
			continue
		}
		v, err := vector.DecodeEmbedding(emb)
		if err != nil {
			return 0, err
		}
		ids = append(ids, id)
		vecs = append(vecs, v)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	idx := &bruteforce.Index{}
	if err := idx.Build(ids, vecs); err != nil {
		return 0, err
	}
	data, err := idx.MarshalBinary()
	if err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO vector_storage(shadow_table_name, dataset_id, "index") VALUES(?, ?, ?)`, shadow, dataset, data); err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func parseTarget(arg string) (string, string, error) {
	parts := strings.SplitN(arg, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("vec_admin: MATCH expects 'shadow:dataset', got %q", arg)
	}
	shadow := strings.TrimSpace(parts[0])
	dataset := strings.TrimSpace(parts[1])
	if shadow == "" || dataset == "" {
		return "", "", fmt.Errorf("vec_admin: MATCH expects 'shadow:dataset', got %q", arg)
	}
	return shadow, dataset, nil
}
