package vec

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"

	idxapi "github.com/viant/sqlite-vec/index"
	"github.com/viant/sqlite-vec/index/bruteforce"
	covidx "github.com/viant/sqlite-vec/index/cover"
	"github.com/viant/sqlite-vec/vector"
	sqlite "modernc.org/sqlite"
	"modernc.org/sqlite/vtab"
)

// Module implements vtab.Module for the vec virtual table. It creates a
// per-table shadow store and supports MATCH-based similarity scans.
type Module struct {
	db *sql.DB
}

// Table represents a single vec virtual table instance.
type Table struct {
	db        *sql.DB
	dbName    string
	tableName string
	shadow    string // qualified shadow table name (e.g. "main._vec_docs")

	mu  sync.RWMutex
	idx idxapi.Index // cached in-memory index loaded from vector_storage or built from shadow

	indexKind string // "brute" (default) or "cover"
}

// Global registry of active tables keyed by shadow name for cache invalidation.
var registry = struct {
	mu       sync.RWMutex
	byShadow map[string]map[*Table]struct{}
}{byShadow: make(map[string]map[*Table]struct{})}

var registerInvalidateOnce sync.Once

func trackTable(t *Table) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	set := registry.byShadow[t.shadow]
	if set == nil {
		set = make(map[*Table]struct{})
		registry.byShadow[t.shadow] = set
	}
	set[t] = struct{}{}
}

func untrackTable(t *Table) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	set := registry.byShadow[t.shadow]
	if set == nil {
		return
	}
	delete(set, t)
	if len(set) == 0 {
		delete(registry.byShadow, t.shadow)
	}
}

// InvalidateCache clears cached indices for a given shadow across active tables.
func InvalidateCache(shadow string) int {
	registry.mu.RLock()
	set := registry.byShadow[shadow]
	var tables []*Table
	for t := range set {
		tables = append(tables, t)
	}
	registry.mu.RUnlock()
	count := 0
	for _, t := range tables {
		t.mu.Lock()
		if t.idx != nil {
			t.idx = nil
			count++
		}
		t.mu.Unlock()
	}
	return count
}

// invalidateFunc implements SQL scalar vec_invalidate(shadow TEXT) → INT.
func invalidateFunc(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 1 {
		return int64(0), nil
	}
	var s string
	switch v := args[0].(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	case nil:
		return int64(0), nil
	default:
		return int64(0), nil
	}
	n := InvalidateCache(s)
	return int64(n), nil
}

// Cursor scans results from a vec table.
type Cursor struct {
	table *Table
	rows  []struct {
		rowid int64
		id    string
	}
	pos int
}

// Register registers the vec virtual table module with the provided *sql.DB.
func Register(db *sql.DB) error {
    mod := &Module{db: db}
    if err := vtab.RegisterModule(db, "vec", mod); err != nil {
        if !strings.Contains(err.Error(), "already registered") {
            return err
        }
    }
	// Register vec_invalidate globally for new connections; idempotent.
	registerInvalidateOnce.Do(func() { _ = sqlite.RegisterDeterministicScalarFunction("vec_invalidate", 1, invalidateFunc) })
	return nil
}

// Create initializes a vec table instance and ensures shadow/index tables.
func (m *Module) Create(ctx vtab.Context, args []string) (vtab.Table, error) {
    if len(args) < 3 {
        return nil, fmt.Errorf("vec: CREATE expects at least 3 args, got %d", len(args))
    }
    // Determine declared column name from args (e.g. USING vec(value)).
    col := "value"
    optStart := 3
    if len(args) > 3 {
        a := strings.TrimSpace(args[3])
        if a != "" && !strings.Contains(a, "=") {
            col = a
            optStart = 4
        }
    }
    // Declare the virtual table schema with a single TEXT column.
    if err := ctx.Declare(fmt.Sprintf("CREATE TABLE %s(%s)", args[2], col)); err != nil {
        return nil, err
    }
    t := &Table{db: m.db, dbName: args[1], tableName: args[2]}
    // Initialize shadow name eagerly so subsequent statements on the same connection work.
    t.shadow = t.qualifiedShadow()
    t.indexKind = parseIndexKind(args[optStart:])
    // Defer vector_storage creation until first use to avoid cross-connection DDL during xCreate.
    trackTable(t)
    return t, nil
}

// Connect attaches to an existing vec table instance.
func (m *Module) Connect(ctx vtab.Context, args []string) (vtab.Table, error) {
    if len(args) < 3 {
        return nil, fmt.Errorf("vec: CONNECT expects at least 3 args, got %d", len(args))
    }
    // Determine declared column name from args (e.g. USING vec(value)).
    col := "value"
    optStart := 3
    if len(args) > 3 {
        a := strings.TrimSpace(args[3])
        if a != "" && !strings.Contains(a, "=") {
            col = a
            optStart = 4
        }
    }
    // Declare the virtual table schema with a single TEXT column.
    if err := ctx.Declare(fmt.Sprintf("CREATE TABLE %s(%s)", args[2], col)); err != nil {
        return nil, err
    }
    t := &Table{db: m.db, dbName: args[1], tableName: args[2]}
    // Reconstruct shadow name and ensure vector_storage exists.
    t.shadow = t.qualifiedShadow()
    // Parse options if provided (args mirror Create's argv).
    t.indexKind = parseIndexKind(args[optStart:])
    // Defer vector_storage creation until first use.
    trackTable(t)
    return t, nil
}

// BestIndex pushes down MATCH on first column.
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

// Open allocates a new cursor.
func (t *Table) Open() (vtab.Cursor, error) { return &Cursor{table: t}, nil }

// Disconnect cleans up per-connection resources.
func (t *Table) Disconnect() error { untrackTable(t); return nil }

// Destroy drops nothing for now; shadow persists.
func (t *Table) Destroy() error { untrackTable(t); return nil }

// Filter computes the result set based on idxNum/vals.
func (c *Cursor) Filter(idxNum int, idxStr string, vals []vtab.Value) error {
	_ = idxStr
	if c.table == nil || c.table.db == nil {
		c.rows = nil
		c.pos = 0
		return nil
	}
	ctx := context.Background()

	// No MATCH: return stable ordering by rowid from shadow.
	if idxNum != 1 || len(vals) == 0 || vals[0] == nil {
		q := fmt.Sprintf("SELECT rowid, id FROM %s ORDER BY rowid", c.table.shadow)
		rows, err := c.table.db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		var out []struct {
			rowid int64
			id    string
		}
		for rows.Next() {
			var r struct {
				rowid int64
				id    string
			}
			if err := rows.Scan(&r.rowid, &r.id); err != nil {
				return err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		c.rows = out
		c.pos = 0
		return nil
	}

	// MATCH: vals[0] is a BLOB with encoded embedding.
	qBlob, ok := vals[0].([]byte)
	if !ok {
		return fmt.Errorf("vec: expected MATCH arg as BLOB, got %T", vals[0])
	}
	qEmb, err := vector.DecodeEmbedding(qBlob)
	if err != nil {
		return err
	}

	// Try to use a cached/persisted index; build and persist if missing.
	idx, err := c.table.ensureIndex(ctx)
	if err != nil {
		return err
	}
	// Query with k = all for now; SQLite LIMIT will trim afterwards.
	ids, _, err := idx.Query(qEmb, 0)
	if err != nil {
		return err
	}
	// Map ids to rowids; simple per-id lookup.
	out := make([]struct {
		rowid int64
		id    string
	}, 0, len(ids))
	for _, id := range ids {
		rid, err := c.table.lookupRowID(ctx, id)
		if err != nil {
			return err
		}
		if rid == 0 {
			continue
		}
		out = append(out, struct {
			rowid int64
			id    string
		}{rowid: rid, id: id})
	}
	c.rows = out
	c.pos = 0
	return nil
}

// Next advances the cursor.
func (c *Cursor) Next() error {
	if c.pos < len(c.rows) {
		c.pos++
	}
	return nil
}

// Eof reports end-of-rows.
func (c *Cursor) Eof() bool { return c.pos >= len(c.rows) }

// Column returns the value of a column in the current row.
func (c *Cursor) Column(col int) (vtab.Value, error) {
	if c.pos < 0 || c.pos >= len(c.rows) {
		return nil, fmt.Errorf("vec: Column out of range (pos=%d,len=%d)", c.pos, len(c.rows))
	}
	if col == 0 {
		return c.rows[c.pos].id, nil
	}
	return nil, nil
}

// Rowid returns the current rowid.
func (c *Cursor) Rowid() (int64, error) {
	if c.pos < 0 || c.pos >= len(c.rows) {
		return 0, fmt.Errorf("vec: Rowid out of range (pos=%d,len=%d)", c.pos, len(c.rows))
	}
	return c.rows[c.pos].rowid, nil
}

// Close releases resources.
func (c *Cursor) Close() error { c.rows = nil; c.pos = 0; return nil }

// ensureShadow ensures the per-table shadow table exists.
func (t *Table) ensureShadow() error {
	if t.db == nil {
		return fmt.Errorf("vec: db is nil")
	}
	name := t.qualifiedShadow()
	t.shadow = name
	// Basic payload columns follow the existing vector schema for compatibility.
	stmt := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    id TEXT PRIMARY KEY,
    content TEXT,
    meta TEXT,
    embedding BLOB
);
`, name)
	_, err := t.db.Exec(stmt)
	if err != nil {
		return err
	}
	// Create triggers to invalidate persisted index on any shadow change.
	// We delete the vector_storage row for this shadow, causing ensureIndex to rebuild on next use.
	trigBase := sanitizeName("trg_vec_" + t.shadow)
	// Embed the shadow name as a SQL string literal in trigger body to avoid parameter issues in DDL.
	shadowLit := quoteLiteral(t.shadow)
	delSQL := `DELETE FROM vector_storage WHERE shadow_table_name = ` + shadowLit + `;`
	invSQL := `SELECT vec_invalidate(` + shadowLit + `);`
	// AFTER INSERT
	stmtIns := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ins AFTER INSERT ON %s BEGIN %s %s END;`, trigBase, name, delSQL, invSQL)
	if _, err := t.db.Exec(stmtIns); err != nil {
		return err
	}
	// AFTER UPDATE
	stmtUpd := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_upd AFTER UPDATE ON %s BEGIN %s %s END;`, trigBase, name, delSQL, invSQL)
	if _, err := t.db.Exec(stmtUpd); err != nil {
		return err
	}
	// AFTER DELETE
	stmtDel := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_del AFTER DELETE ON %s BEGIN %s %s END;`, trigBase, name, delSQL, invSQL)
	if _, err := t.db.Exec(stmtDel); err != nil {
		return err
	}
	return nil
}

// ensureVectorStorage ensures the shared vector_storage table exists.
func ensureVectorStorage(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("vec: db is nil")
	}
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS vector_storage (
    shadow_table_name TEXT PRIMARY KEY,
    "index" BLOB
)`)
	return err
}

// qualifiedShadow returns a fully-qualified shadow table name.
func (t *Table) qualifiedShadow() string {
	// Use a deterministic shadow name to avoid clashes, prefixed with _vec_.
	base := "_vec_" + t.tableName
	if strings.TrimSpace(t.dbName) == "" {
		return base
	}
	return t.dbName + "." + base
}

// ensureIndex loads or builds an in-memory index and persists it in vector_storage.
func (t *Table) ensureIndex(ctx context.Context) (idxapi.Index, error) {
	// First consult persistent storage to see if an index exists or was invalidated by triggers.
	var blob []byte
	err := t.db.QueryRowContext(ctx, `SELECT "index" FROM vector_storage WHERE shadow_table_name = ?`, t.shadow).Scan(&blob)
	hasPersisted := (err == nil && len(blob) > 0)
	if hasPersisted {
		t.mu.RLock()
		if t.idx != nil {
			t.mu.RUnlock()
			return t.idx, nil
		}
		t.mu.RUnlock()
		// No cached index; load from storage using selected backend.
		var idx idxapi.Index
		switch t.indexKind {
		case "cover":
			c := &covidx.Index{}
			if err := c.UnmarshalBinary(blob); err == nil {
				idx = c
			}
		default:
			b := &bruteforce.Index{}
			if err := b.UnmarshalBinary(blob); err == nil {
				idx = b
			}
		}
		if idx != nil {
			t.mu.Lock()
			t.idx = idx
			t.mu.Unlock()
			return idx, nil
		}
		// fall through to rebuild on decode error
	}

	// Either no persisted index or decode failed: rebuild.
	t.mu.Lock()
	defer t.mu.Unlock()

	// Build from shadow rows
	q := fmt.Sprintf("SELECT id, embedding FROM %s WHERE embedding IS NOT NULL", t.shadow)
	rows, err := t.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	var vecs [][]float32
	for rows.Next() {
		var id string
		var emb []byte
		if err := rows.Scan(&id, &emb); err != nil {
			return nil, err
		}
		if len(emb) == 0 {
			continue
		}
		v, err := vector.DecodeEmbedding(emb)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
		vecs = append(vecs, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var built idxapi.Index
	// Choose backend
	switch t.indexKind {
	case "cover":
		c := &covidx.Index{}
		if err := c.Build(ids, vecs); err != nil {
			return nil, err
		}
		built = c
		t.idx = c
	default:
		bf := &bruteforce.Index{}
		if err := bf.Build(ids, vecs); err != nil {
			return nil, err
		}
		built = bf
		t.idx = bf
	}
	// Persist
	data, err := built.MarshalBinary()
	if err == nil {
		_, _ = t.db.ExecContext(ctx, `INSERT OR REPLACE INTO vector_storage(shadow_table_name, "index") VALUES(?, ?)`, t.shadow, data)
	}
	return t.idx, nil
}

// lookupRowID resolves a rowid for a given id from the shadow table.
func (t *Table) lookupRowID(ctx context.Context, id string) (int64, error) {
	q := fmt.Sprintf("SELECT rowid FROM %s WHERE id = ?", t.shadow)
	row := t.db.QueryRowContext(ctx, q, id)
	var rid int64
	if err := row.Scan(&rid); err != nil {
		return 0, err
	}
	return rid, nil
}

// sanitizeName converts a qualified name into a safe identifier for triggers.
func sanitizeName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch r {
		case '.', '-', ' ':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// quoteLiteral returns SQL string literal with single quotes escaped for safe embedding.
func quoteLiteral(s string) string {
	// Replace ' with '' per SQL string literal rules.
	escaped := strings.ReplaceAll(s, "'", "''")
	return "'" + escaped + "'"
}

func parseIndexKind(args []string) string {
	kind := "brute"
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "index=") {
			v := strings.TrimSpace(strings.TrimPrefix(a, "index="))
			switch v {
			case "cover", "brute":
				kind = v
			}
		}
	}
	return kind
}
