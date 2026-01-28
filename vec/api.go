package vec

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

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

	dbPathOnce sync.Once
	dbPathErr  error
	dbPath     string

	indexKind string // "auto" (default), "brute", or "cover"
	coverOpts coverOptions
}

type indexOptions struct {
	kind  string
	cover coverOptions
}

type coverOptions struct {
	base         float32
	useBound     bool
	bound        covidx.BoundStrategy
	useDistance  bool
	distance     covidx.DistanceFunction
	parallel     int
	autoParallel bool
}

const (
	defaultIndexKind            = "auto"
	autoCoverMinDocs            = 4000
	autoCoverMinDim             = 64
	autoCoverMinDensity float64 = 16
)

func (c coverOptions) toIndexOptions() []covidx.Option {
	var opts []covidx.Option
	if c.base > 1 {
		opts = append(opts, covidx.WithBase(c.base))
	}
	if c.useBound {
		opts = append(opts, covidx.WithBoundStrategy(c.bound))
	}
	if c.useDistance {
		opts = append(opts, covidx.WithDistance(c.distance))
	}
	switch {
	case c.autoParallel:
		opts = append(opts, covidx.WithBuildParallelism(runtime.GOMAXPROCS(0)))
	case c.parallel > 0:
		opts = append(opts, covidx.WithBuildParallelism(c.parallel))
	}
	return opts
}

func (t *Table) newCoverIndex() *covidx.Index {
	return covidx.New(t.coverOpts.toIndexOptions()...)
}

func (t *Table) resolveIndexKind(docCount, dim int) string {
	switch t.indexKind {
	case "cover", "brute":
		return t.indexKind
	case "auto", "":
	default:
		if t.indexKind != "auto" && t.indexKind != "" {
			return t.indexKind
		}
	}
	if docCount >= autoCoverMinDocs && dim >= autoCoverMinDim {
		if dim > 0 {
			density := float64(docCount) / float64(dim)
			if density >= autoCoverMinDensity {
				return "cover"
			}
		}
	}
	return "brute"
}

func parseIndexOptions(args []string) indexOptions {
	opts := indexOptions{
		kind:  defaultIndexKind,
		cover: coverOptions{},
	}
	for _, raw := range args {
		a := strings.TrimSpace(raw)
		if a == "" {
			continue
		}
		parts := strings.SplitN(a, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		switch key {
		case "index":
			switch strings.ToLower(val) {
			case "cover", "brute", "auto":
				opts.kind = strings.ToLower(val)
			}
		case "cover_base":
			if f, err := strconv.ParseFloat(val, 32); err == nil && f > 1 {
				opts.cover.base = float32(f)
			}
		case "cover_bound":
			switch strings.ToLower(val) {
			case "level", "boundlevel":
				opts.cover.bound = covidx.BoundLevel
				opts.cover.useBound = true
			case "per_node", "pernode", "node":
				opts.cover.bound = covidx.BoundPerNode
				opts.cover.useBound = true
			}
		case "cover_distance":
			switch strings.ToLower(val) {
			case "cos", "cosine":
				opts.cover.distance = covidx.DistanceFunctionCosine
				opts.cover.useDistance = true
			case "l2", "euclidean":
				opts.cover.distance = covidx.DistanceFunctionEuclidean
				opts.cover.useDistance = true
			}
		case "cover_parallel":
			lower := strings.ToLower(val)
			switch lower {
			case "", "0", "off":
				opts.cover.parallel = 0
				opts.cover.autoParallel = false
			case "auto":
				opts.cover.autoParallel = true
				opts.cover.parallel = 0
			default:
				if n, err := strconv.Atoi(lower); err == nil {
					if n < 0 {
						n = 0
					}
					opts.cover.parallel = n
					opts.cover.autoParallel = false
				}
			}
		}
	}
	return opts
}

// Global shared cache of indices keyed by db path/table/dataset for cross-connection reuse.
var sharedCache = struct {
	mu    sync.RWMutex
	byKey map[string]*cacheEntry
}{byKey: make(map[string]*cacheEntry)}

var registerInvalidateOnce sync.Once

type cacheEntry struct {
	mu       sync.RWMutex
	idx      idxapi.Index
	building bool
	cond     *sync.Cond
}

func newCacheEntry() *cacheEntry {
	e := &cacheEntry{}
	e.cond = sync.NewCond(&e.mu)
	return e
}

func (e *cacheEntry) get() idxapi.Index {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.idx
}

func (e *cacheEntry) set(idx idxapi.Index) {
	e.mu.Lock()
	e.idx = idx
	e.mu.Unlock()
}

func (e *cacheEntry) waitForBuild() idxapi.Index {
	e.mu.Lock()
	for e.building {
		e.cond.Wait()
	}
	idx := e.idx
	e.mu.Unlock()
	return idx
}

func (e *cacheEntry) startBuild() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.idx != nil || e.building {
		return false
	}
	e.building = true
	return true
}

func (e *cacheEntry) finishBuild() {
	e.mu.Lock()
	e.building = false
	e.cond.Broadcast()
	e.mu.Unlock()
}

func cacheKey(dbPath, tableName, dataset string) string {
	return dbPath + "|" + tableName + "|" + dataset
}

func getCacheEntry(key string) *cacheEntry {
	sharedCache.mu.RLock()
	entry := sharedCache.byKey[key]
	sharedCache.mu.RUnlock()
	if entry != nil {
		return entry
	}
	sharedCache.mu.Lock()
	defer sharedCache.mu.Unlock()
	if entry = sharedCache.byKey[key]; entry == nil {
		entry = newCacheEntry()
		sharedCache.byKey[key] = entry
	}
	return entry
}

func getSharedIndex(key string) idxapi.Index {
	sharedCache.mu.RLock()
	entry := sharedCache.byKey[key]
	sharedCache.mu.RUnlock()
	if entry == nil {
		return nil
	}
	return entry.get()
}

func setSharedIndex(entry *cacheEntry, idx idxapi.Index) {
	entry.set(idx)
}

// InvalidateCache clears cached indices for a given shadow/dataset across active connections.
func InvalidateCache(shadow, dataset string) int {
	sharedCache.mu.Lock()
	defer sharedCache.mu.Unlock()
	count := 0
	tableName := tableNameFromShadow(shadow)
	if tableName == "" {
		tableName = shadow
	}
	if dataset == "" {
		pattern := "|" + tableName + "|"
		for k, entry := range sharedCache.byKey {
			if strings.Contains(k, pattern) {
				entry.set(nil)
				count++
			}
		}
		return count
	}
	suffix := "|" + tableName + "|" + dataset
	for k, entry := range sharedCache.byKey {
		if strings.HasSuffix(k, suffix) {
			entry.set(nil)
			count++
		}
	}
	return count
}

// invalidateFunc implements SQL scalar vec_invalidate(shadow TEXT, dataset TEXT) → INT.
func invalidateFunc(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 {
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
	ds, err := asString(args[1])
	if err != nil {
		return int64(0), nil
	}
	n := InvalidateCache(s, ds)
	return int64(n), nil
}

const (
	idxDatasetScan = iota
	idxDatasetMatch
	idxDatasetMatchScore
)

// Cursor scans results from a vec table.
type Cursor struct {
	table *Table
	rows  []struct {
		rowid   int64
		dataset string
		id      string
		score   float64
	}
	pos      int
	minScore *float64
	dataset  string
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
	registerInvalidateOnce.Do(func() { _ = sqlite.RegisterDeterministicScalarFunction("vec_invalidate", 2, invalidateFunc) })
	return nil
}

// Create initializes a vec table instance and ensures shadow/index tables.
func (m *Module) Create(ctx vtab.Context, args []string) (vtab.Table, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("vec: CREATE expects at least 3 args, got %d", len(args))
	}
	if err := ctx.EnableConstraintSupport(); err != nil {
		return nil, fmt.Errorf("vec: EnableConstraintSupport failed: %w", err)
	}
	// Determine declared column name from args (e.g. USING vec(doc_id)).
	col := "doc_id"
	optStart := 3
	if len(args) > 3 {
		a := strings.TrimSpace(args[3])
		if a != "" && !strings.Contains(a, "=") {
			col = a
			optStart = 4
		}
	}
	// Declare the virtual table schema with typed columns and a hidden score.
	if err := ctx.Declare(fmt.Sprintf("CREATE TABLE %s(dataset_id TEXT, %s TEXT, match_score REAL HIDDEN)", args[2], col)); err != nil {
		return nil, err
	}
	opts := parseIndexOptions(args[optStart:])
	t := &Table{db: m.db, dbName: args[1], tableName: args[2]}
	// Initialize shadow name eagerly so subsequent statements on the same connection work.
	t.shadow = t.qualifiedShadow()
	t.indexKind = opts.kind
	t.coverOpts = opts.cover
	// Defer vector_storage creation until first use to avoid cross-connection DDL during xCreate.
	return t, nil
}

// Connect attaches to an existing vec table instance.
func (m *Module) Connect(ctx vtab.Context, args []string) (vtab.Table, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("vec: CONNECT expects at least 3 args, got %d", len(args))
	}
	if err := ctx.EnableConstraintSupport(); err != nil {
		return nil, fmt.Errorf("vec: EnableConstraintSupport failed: %w", err)
	}
	// Determine declared column name from args (e.g. USING vec(value)).
	col := "doc_id"
	optStart := 3
	if len(args) > 3 {
		a := strings.TrimSpace(args[3])
		if a != "" && !strings.Contains(a, "=") {
			col = a
			optStart = 4
		}
	}
	// Declare the virtual table schema with typed columns and a hidden score.
	if err := ctx.Declare(fmt.Sprintf("CREATE TABLE %s(dataset_id TEXT, %s TEXT, match_score REAL HIDDEN)", args[2], col)); err != nil {
		return nil, err
	}
	opts := parseIndexOptions(args[optStart:])
	t := &Table{db: m.db, dbName: args[1], tableName: args[2]}
	// Reconstruct shadow name and ensure vector_storage exists.
	t.shadow = t.qualifiedShadow()
	// Parse options if provided (args mirror Create's argv).
	t.indexKind = opts.kind
	t.coverOpts = opts.cover
	// Defer vector_storage creation until first use.
	return t, nil
}

// BestIndex pushes down MATCH on first column.
func (t *Table) BestIndex(info *vtab.IndexInfo) error {
	var (
		datasetConstraint *vtab.Constraint
		matchConstraint   *vtab.Constraint
		scoreConstraint   *vtab.Constraint
		nextArg           int
	)

	for i := range info.Constraints {
		c := &info.Constraints[i]
		if !c.Usable {
			continue
		}
		switch {
		case c.Column == 0 && c.Op == vtab.OpEQ:
			datasetConstraint = c
		case c.Column == 1 && c.Op == vtab.OpMATCH:
			matchConstraint = c
		case c.Column == 2 && (c.Op == vtab.OpGE || c.Op == vtab.OpGT):
			scoreConstraint = c
		}
	}

	if matchConstraint != nil && datasetConstraint == nil {
		return fmt.Errorf("vec: dataset_id constraint is required with MATCH")
	}

	if datasetConstraint != nil {
		datasetConstraint.ArgIndex = nextArg
		datasetConstraint.Omit = true
		nextArg++
	}

	switch {
	case datasetConstraint != nil && matchConstraint == nil:
		info.IdxNum = idxDatasetScan
	case datasetConstraint != nil && matchConstraint != nil && scoreConstraint == nil:
		matchConstraint.ArgIndex = nextArg
		matchConstraint.Omit = true
		nextArg++
		info.IdxNum = idxDatasetMatch
	case datasetConstraint != nil && matchConstraint != nil && scoreConstraint != nil:
		matchConstraint.ArgIndex = nextArg
		matchConstraint.Omit = true
		nextArg++
		scoreConstraint.ArgIndex = nextArg
		nextArg++
		scoreConstraint.Omit = true
		info.IdxNum = idxDatasetMatchScore
	default:
		// No usable plan
		return fmt.Errorf("vec: dataset_id constraint required")
	}

	return nil
}

// Open allocates a new cursor.
func (t *Table) Open() (vtab.Cursor, error) { return &Cursor{table: t}, nil }

// Disconnect cleans up per-connection resources.
func (t *Table) Disconnect() error { return nil }

// Destroy drops nothing for now; shadow persists.
func (t *Table) Destroy() error { return nil }

// Filter computes the result set based on idxNum/vals.
func (c *Cursor) Filter(idxNum int, idxStr string, vals []vtab.Value) error {
	_ = idxStr
	c.minScore = nil
	c.dataset = ""
	if c.table == nil || c.table.db == nil {
		c.rows = nil
		c.pos = 0
		return nil
	}
	ctx := context.Background()

	switch idxNum {
	case idxDatasetScan:
		if len(vals) == 0 || vals[0] == nil {
			return fmt.Errorf("vec: dataset_id argument is required")
		}
		dataset, err := asString(vals[0])
		if err != nil {
			return err
		}
		c.dataset = dataset
		q := fmt.Sprintf("SELECT rowid, dataset_id, id FROM %s WHERE dataset_id = ? ORDER BY rowid", c.table.shadow)
		rows, err := c.table.db.QueryContext(ctx, q, dataset)
		if err != nil {
			return err
		}
		defer rows.Close()
		var out []struct {
			rowid   int64
			dataset string
			id      string
			score   float64
		}
		for rows.Next() {
			var r struct {
				rowid   int64
				dataset string
				id      string
			}
			if err := rows.Scan(&r.rowid, &r.dataset, &r.id); err != nil {
				return err
			}
			out = append(out, struct {
				rowid   int64
				dataset string
				id      string
				score   float64
			}{rowid: r.rowid, dataset: r.dataset, id: r.id, score: 0})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		c.rows = out
		c.pos = 0
		return nil
	case idxDatasetMatch, idxDatasetMatchScore:
		if len(vals) < 2 || vals[0] == nil || vals[1] == nil {
			return fmt.Errorf("vec: dataset_id and MATCH arguments are required")
		}
		dataset, err := asString(vals[0])
		if err != nil {
			return err
		}
		c.dataset = dataset

		qEmb, err := decodeMatchArg(vals[1])
		if err != nil {
			return err
		}

		if idxNum == idxDatasetMatchScore {
			if len(vals) < 3 {
				return fmt.Errorf("vec: missing match_score constraint")
			}
			min, err := asFloat(vals[2])
			if err != nil {
				return err
			}
			c.minScore = &min
		}

		idx, err := c.table.ensureIndex(ctx, c.dataset)
		if err != nil {
			return err
		}
		ids, scores, err := idx.Query(qEmb, 0)
		if err != nil {
			return err
		}

		out := make([]struct {
			rowid   int64
			dataset string
			id      string
			score   float64
		}, 0, len(ids))
		for i, id := range ids {
			var score float64
			if len(scores) > i {
				score = scores[i]
			}
			if c.minScore != nil && score < *c.minScore {
				continue
			}
			rid, err := c.table.lookupRow(ctx, c.dataset, id)
			if err != nil {
				return err
			}
			if rid == 0 {
				continue
			}
			out = append(out, struct {
				rowid   int64
				dataset string
				id      string
				score   float64
			}{rowid: rid, dataset: c.dataset, id: id, score: score})
		}
		c.rows = out
		c.pos = 0
		return nil
	default:
		return fmt.Errorf("vec: unsupported query plan")
	}
}

func decodeMatchArg(v interface{}) ([]float32, error) {
	switch val := v.(type) {
	case []byte:
		return vector.DecodeEmbedding(val)
	case string:
		return decodeMatchString(val)
	default:
		return nil, fmt.Errorf("vec: expected MATCH arg as BLOB or string, got %T", v)
	}
}

func decodeMatchString(raw string) ([]float32, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("vec: MATCH string is empty")
	}
	if strings.HasPrefix(s, "[") {
		var floats []float64
		if err := json.Unmarshal([]byte(s), &floats); err == nil {
			vec := make([]float32, len(floats))
			for i, f := range floats {
				vec[i] = float32(f)
			}
			return vec, nil
		}
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		if vec, err := vector.DecodeEmbedding(b); err == nil {
			return vec, nil
		}
	}
	if strings.Contains(s, ",") {
		parts := strings.Split(s, ",")
		vec := make([]float32, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			f, err := strconv.ParseFloat(p, 32)
			if err != nil {
				return nil, fmt.Errorf("vec: invalid MATCH float %q: %w", p, err)
			}
			vec = append(vec, float32(f))
		}
		if len(vec) > 0 {
			return vec, nil
		}
	}
	return nil, fmt.Errorf("vec: MATCH string must be base64-encoded embedding or JSON/CSV float list")
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
		return c.rows[c.pos].dataset, nil
	}
	if col == 1 {
		return c.rows[c.pos].id, nil
	}
	if col == 2 {
		return c.rows[c.pos].score, nil
	}
	return nil, fmt.Errorf("vec: unsupported column %d", col)
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
    dataset_id TEXT NOT NULL,
    id TEXT NOT NULL,
    content TEXT,
    meta TEXT,
    embedding BLOB,
    PRIMARY KEY(dataset_id, id)
);
`, name)
	_, err := t.db.Exec(stmt)
	if err != nil {
		return err
	}
	// Create triggers to invalidate persisted index on any shadow change.
	// We delete the vector_storage row for this shadow, causing ensureIndex to rebuild on next use.
	trigBase := sanitizeName("trg_vec_" + t.shadow)
	shadowLit := quoteLiteral(t.shadow)
	delNew := `DELETE FROM vector_storage WHERE shadow_table_name = ` + shadowLit + ` AND dataset_id = NEW.dataset_id;`
	invNew := `SELECT vec_invalidate(` + shadowLit + `, NEW.dataset_id);`
	delOld := `DELETE FROM vector_storage WHERE shadow_table_name = ` + shadowLit + ` AND dataset_id = OLD.dataset_id;`
	invOld := `SELECT vec_invalidate(` + shadowLit + `, OLD.dataset_id);`
	// AFTER INSERT
	stmtIns := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ins AFTER INSERT ON %s BEGIN %s %s END;`, trigBase, name, delNew, invNew)
	if _, err := t.db.Exec(stmtIns); err != nil {
		return err
	}
	// AFTER UPDATE - invalidate both NEW and OLD datasets (handles dataset moves).
	stmtUpd := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_upd AFTER UPDATE ON %s BEGIN %s %s %s %s END;`, trigBase, name, delNew, invNew, delOld, invOld)
	if _, err := t.db.Exec(stmtUpd); err != nil {
		return err
	}
	// AFTER DELETE
	stmtDel := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_del AFTER DELETE ON %s BEGIN %s %s END;`, trigBase, name, delOld, invOld)
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
    shadow_table_name TEXT NOT NULL,
    dataset_id        TEXT NOT NULL DEFAULT '',
    "index"           BLOB,
    PRIMARY KEY (shadow_table_name, dataset_id)
)`)
	return err
}

func ensureVectorStorageLocks(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("vec: db is nil")
	}
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS vector_storage_locks (
    shadow_table_name TEXT NOT NULL,
    dataset_id        TEXT NOT NULL DEFAULT '',
    owner             TEXT NOT NULL,
    locked_at         INTEGER NOT NULL,
    PRIMARY KEY (shadow_table_name, dataset_id)
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

func tableNameFromShadow(shadow string) string {
	if shadow == "" {
		return ""
	}
	if i := strings.Index(shadow, "._vec_"); i >= 0 {
		return shadow[i+len("._vec_"):]
	}
	if strings.HasPrefix(shadow, "_vec_") {
		return strings.TrimPrefix(shadow, "_vec_")
	}
	return ""
}

func resolveDbPath(ctx context.Context, db *sql.DB, dbName string) (string, error) {
	if db == nil {
		return "", fmt.Errorf("vec: db is nil")
	}
	rows, err := db.QueryContext(ctx, `SELECT name, file FROM pragma_database_list`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name, file string
		if err := rows.Scan(&name, &file); err != nil {
			return "", err
		}
		if dbName == "" {
			if name == "main" {
				if file == "" {
					return name, nil
				}
				return file, nil
			}
		} else if name == dbName {
			if file == "" {
				return name, nil
			}
			return file, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if dbName != "" {
		return dbName, nil
	}
	return "main", nil
}

func (t *Table) cachedDbPath(ctx context.Context) string {
	t.dbPathOnce.Do(func() {
		path, err := resolveDbPath(ctx, t.db, t.dbName)
		if err != nil {
			t.dbPathErr = err
			if t.dbName != "" {
				t.dbPath = t.dbName
			} else {
				t.dbPath = "main"
			}
			return
		}
		t.dbPath = path
	})
	return t.dbPath
}

func (t *Table) loadPersistedIndex(ctx context.Context, dataset string) (idxapi.Index, bool, error) {
	var blob []byte
	err := t.db.QueryRowContext(ctx, `SELECT "index" FROM vector_storage WHERE shadow_table_name = ? AND dataset_id = ?`, t.shadow, dataset).Scan(&blob)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(blob) == 0 {
		return nil, false, nil
	}
	if isCoverBlob(blob) {
		c := t.newCoverIndex()
		if err := c.UnmarshalBinary(blob); err == nil {
			return c, true, nil
		}
		return nil, false, nil
	}
	b := &bruteforce.Index{}
	if err := b.UnmarshalBinary(blob); err == nil {
		return b, true, nil
	}
	return nil, false, nil
}

const (
	lockRetryDelay = 50 * time.Millisecond
	lockStaleAfter = 2 * time.Minute
)

var lockOwnerID = fmt.Sprintf("pid:%d-%d", os.Getpid(), time.Now().UnixNano())

func acquireIndexBuildLock(ctx context.Context, db *sql.DB, shadow, dataset string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := time.Now().Unix()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO vector_storage_locks(shadow_table_name, dataset_id, owner, locked_at) VALUES(?, ?, ?, ?)`, shadow, dataset, lockOwnerID, now); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		var owner string
		var lockedAt int64
		if err := tx.QueryRowContext(ctx, `SELECT owner, locked_at FROM vector_storage_locks WHERE shadow_table_name = ? AND dataset_id = ?`, shadow, dataset).Scan(&owner, &lockedAt); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if owner != lockOwnerID && lockedAt <= time.Now().Add(-lockStaleAfter).Unix() {
			res, err := tx.ExecContext(ctx, `UPDATE vector_storage_locks SET owner = ?, locked_at = ? WHERE shadow_table_name = ? AND dataset_id = ? AND locked_at = ?`, lockOwnerID, now, shadow, dataset, lockedAt)
			if err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				owner = lockOwnerID
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		if owner == lockOwnerID {
			return func() {
				_, _ = db.ExecContext(context.Background(), `DELETE FROM vector_storage_locks WHERE shadow_table_name = ? AND dataset_id = ? AND owner = ?`, shadow, dataset, lockOwnerID)
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockRetryDelay):
		}
	}
}

// ensureIndex loads or builds an in-memory index and persists it in vector_storage.
func (t *Table) ensureIndex(ctx context.Context, dataset string) (idxapi.Index, error) {
	if strings.TrimSpace(dataset) == "" {
		return nil, fmt.Errorf("vec: dataset_id is required for ensureIndex")
	}
	if err := t.ensureShadow(); err != nil {
		return nil, err
	}
	if err := ensureVectorStorage(t.db); err != nil {
		return nil, err
	}
	if err := ensureVectorStorageLocks(t.db); err != nil {
		return nil, err
	}

	dbPath := t.cachedDbPath(ctx)
	key := cacheKey(dbPath, t.tableName, dataset)
	entry := getCacheEntry(key)
	if idx := entry.get(); idx != nil {
		return idx, nil
	}

	if idx, ok, err := t.loadPersistedIndex(ctx, dataset); err != nil {
		return nil, err
	} else if ok {
		setSharedIndex(entry, idx)
		return idx, nil
	}

	started := false
	for !started {
		if idx := entry.get(); idx != nil {
			return idx, nil
		}
		if entry.startBuild() {
			started = true
			break
		}
		if idx := entry.waitForBuild(); idx != nil {
			return idx, nil
		}
	}
	defer entry.finishBuild()

	if idx, ok, err := t.loadPersistedIndex(ctx, dataset); err != nil {
		return nil, err
	} else if ok {
		setSharedIndex(entry, idx)
		return idx, nil
	}

	unlock, err := acquireIndexBuildLock(ctx, t.db, t.shadow, dataset)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if idx := entry.get(); idx != nil {
		return idx, nil
	}
	if idx, ok, err := t.loadPersistedIndex(ctx, dataset); err != nil {
		return nil, err
	} else if ok {
		setSharedIndex(entry, idx)
		return idx, nil
	}

	q := fmt.Sprintf("SELECT id, embedding FROM %s WHERE dataset_id = ? AND embedding IS NOT NULL", t.shadow)
	rows, err := t.db.QueryContext(ctx, q, dataset)
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
	dim := 0
	if len(vecs) > 0 {
		dim = len(vecs[0])
	}
	resolved := t.resolveIndexKind(len(ids), dim)
	switch resolved {
	case "cover":
		c := t.newCoverIndex()
		if err := c.Build(ids, vecs); err != nil {
			return nil, err
		}
		built = c
	default:
		bf := &bruteforce.Index{}
		if err := bf.Build(ids, vecs); err != nil {
			return nil, err
		}
		built = bf
	}

	if data, err := built.MarshalBinary(); err == nil {
		_, _ = t.db.ExecContext(ctx, `INSERT OR REPLACE INTO vector_storage(shadow_table_name, dataset_id, "index") VALUES(?, ?, ?)`, t.shadow, dataset, data)
	}
	setSharedIndex(entry, built)
	return built, nil
}

// lookupRow resolves rowid for a given dataset/id pair.
func (t *Table) lookupRow(ctx context.Context, dataset, id string) (int64, error) {
	q := fmt.Sprintf("SELECT rowid FROM %s WHERE dataset_id = ? AND id = ?", t.shadow)
	row := t.db.QueryRowContext(ctx, q, dataset, id)
	var rid int64
	if err := row.Scan(&rid); err != nil {
		return 0, err
	}
	return rid, nil
}

func asFloat(v vtab.Value) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case int64:
		return float64(val), nil
	case []byte:
		f, err := strconv.ParseFloat(string(val), 64)
		if err != nil {
			return 0, fmt.Errorf("vec: cannot parse score %q: %w", string(val), err)
		}
		return f, nil
	case string:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("vec: cannot parse score %q: %w", val, err)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("vec: unsupported score type %T", v)
	}
}

func asString(v vtab.Value) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case []byte:
		return string(val), nil
	case nil:
		return "", fmt.Errorf("vec: dataset_id is nil")
	default:
		return "", fmt.Errorf("vec: unsupported dataset_id type %T", v)
	}
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

func isCoverBlob(blob []byte) bool {
	return len(blob) >= 4 && string(blob[:4]) == "COV1"
}
