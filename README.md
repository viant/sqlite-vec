# sqlite-vec
[![GoReportCard](https://goreportcard.com/badge/github.com/viant/sqlite-vec)](https://goreportcard.com/report/github.com/viant/sqlite-vec)
[![Go Reference](https://pkg.go.dev/badge/github.com/viant/sqlite-vec.svg)](https://pkg.go.dev/github.com/viant/sqlite-vec)

Vector search for SQLite in pure Go. `sqlite-vec` lets you keep embeddings, metadata,
and similarity search inside a single SQLite file—ideal for edge deployments,
serverless functions, and local tooling—while still syncing from an upstream SQL
source of truth.

- **Language:** Go 1.24+
- **Driver:** [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) (CGO-free)
- **Virtual tables:** implemented entirely in Go; no custom SQLite builds required

---

## Table of contents

1. [Architecture](#architecture)
2. [Packages](#packages)
3. [Schemas](#schemas)
4. [Local quick start](#local-quick-start)
5. [Shadow tables & datasets](#shadow-tables--datasets)
6. [Querying with `vec`](#querying-with-vec)
7. [Ingestion & SCN sync](#ingestion--scn-sync)
8. [Administration](#administration)
9. [Troubleshooting](#troubleshooting)
10. [Contributing & license](#contributing--license)

---

## Architecture

```
┌──────────────────────────┐        ┌──────────────────────────┐
│ Upstream SQL (MySQL)     │        │ Downstream SQLite        │
│  - vec_dataset           │  SCN   │  - shadow_vec_docs       │
│  - shadow_vec_docs       │ logs   │  - vec virtual tables    │
│  - vec_shadow_log ◄──────┼────────┤  - vector_storage        │
│  - vec_dataset_scn       │        │  - vec_admin, vecsync    │
└──────────────────────────┘        └──────────────────────────┘
```

1. **Ingestion pipeline** (external project) watches files, chunks documents,
   calls embedding models, and writes rows into upstream `shadow_vec_docs`
   (or per-dataset tables) while producing SCN entries in `vec_shadow_log`.
2. **`vecsync`** (this repo) provides helper DDL + metadata so downstream
   replicas can replay `vec_shadow_log` and keep their local SQLite copy in sync.
3. **`vec` virtual tables** expose `MATCH` queries over the local shadow tables
   and persist vector indexes in `vector_storage`, sharded by dataset.
4. **`vec_admin`** rebuilds indexes on demand per `(shadow_table, dataset)`.

The ingestion service, embedding calls, deduplication logic, and background
sync loop are intentionally out of scope for this repository—you can plug
`sqlite-vec` into whatever pipeline you already have.

---

## Packages

| Package     | Description |
| ----------- | ----------- |
| `engine`    | Thin wrapper around `modernc.org/sqlite` plus optional registration of `vec_cosine` / `vec_l2`. |
| `vec`       | Virtual table module. Provides dataset-aware `MATCH` queries backed by per-table shadow tables and persisted vector indexes. |
| `vecadmin`  | Administrative virtual table. `SELECT op FROM vec_admin WHERE op MATCH 'shadow:dataset'` rebuilds the corresponding index. |
| `vecsync`   | SCN/logging helpers (DDL, trigger builders, data models) for building replication pipelines. |
| `vector`    | Baseline document schema + helpers (`EncodeEmbedding`, cosine/l2 SQL functions). |
| `vecutil`   | Optional text-first helpers (`EmbedFunc`, `UpsertShadowDocument`, `MatchText`, etc.). Useful for demos and tests but not required for production. |

`internal/cover` and `index/*` host experimental index implementations (brute-force,
cover tree) used by the virtual table.

---

## Schemas

Reference DDL lives under `db/schema`:

- `db/schema/mysql/schema.ddl` – base tables for upstream multi-tenant deployments.
- `db/schema/mysql/shadow_sync.ddl` – extended MySQL script with SCN helpers,
  triggers, and logging for `shadow_vec_docs`.
- `db/schema/sqlite/schema.ddl` – downstream replicas or single-node setups.

Tables of note:

- `vec_dataset(dataset_id, description, source_uri, last_scn, ...)`
- `shadow_vec_docs(dataset_id, id, content, meta, embedding, embedding_model, scn, archived, ...)`
- `vec_shadow_log(dataset_id, shadow_table, scn, op, document_id, payload)`
- `vec_sync_state(dataset_id, shadow_table, last_scn)`
- `vector_storage(shadow_table_name, dataset_id, index)`

Every document row **must** include `dataset_id`. The virtual table enforces
`dataset_id = ?` in `WHERE` clauses so indexes and caches stay isolated.

---

## Local quick start

```go
package main

import (
    "log"

    "github.com/viant/sqlite-vec/engine"
    "github.com/viant/sqlite-vec/vec"
    "github.com/viant/sqlite-vec/vector"
)

func main() {
    db, err := engine.Open("./vec_demo.sqlite")
    if err != nil {
        log.Fatalf("engine.Open failed: %v", err)
    }
    defer db.Close()

    // Keep a single underlying connection so module registration is visible.
    db.SetMaxOpenConns(1)

    if err := vec.Register(db); err != nil {
        log.Fatalf("vec.Register failed: %v", err)
    }

    if _, err := db.Exec(`CREATE VIRTUAL TABLE vec_docs USING vec(doc_id)`); err != nil {
        log.Fatalf("CREATE VIRTUAL TABLE failed: %v", err)
    }

    // Create the shadow table explicitly for demos.
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _vec_vec_docs (
        dataset_id TEXT NOT NULL,
        id         TEXT NOT NULL,
        content    TEXT,
        meta       TEXT,
        embedding  BLOB,
        PRIMARY KEY(dataset_id, id)
    )`); err != nil {
        log.Fatalf("create shadow failed: %v", err)
    }

    e1, _ := vector.EncodeEmbedding([]float32{1, 0})
    e2, _ := vector.EncodeEmbedding([]float32{0, 1})
    if _, err := db.Exec(`INSERT INTO _vec_vec_docs(dataset_id, id, embedding) VALUES
        ('demo','a',?),
        ('demo','b',?)`, e1, e2); err != nil {
        log.Fatalf("insert embeddings failed: %v", err)
    }

    q, _ := vector.EncodeEmbedding([]float32{1, 0})
    rows, err := db.Query(`SELECT doc_id, match_score FROM vec_docs
        WHERE dataset_id = 'demo' AND doc_id MATCH ?`, q)
    if err != nil {
        log.Fatalf("MATCH failed: %v", err)
    }
    defer rows.Close()

    for rows.Next() {
        var id string
        var score float64
        if err := rows.Scan(&id, &score); err != nil {
            log.Fatalf("scan failed: %v", err)
        }
        log.Printf("%s score=%.3f", id, score)
    }
}
```

> For real deployments, shadow tables live in MySQL (or another upstream DB) and
> replicate into SQLite via `vecsync`. The snippet above is only for local experiments.

### Virtual table options

Pass tuning flags directly in the `USING vec(...)` clause:

```sql
CREATE VIRTUAL TABLE vec_docs USING vec(
  doc_id,
  dbpath=/tmp/vec_shadow.sqlite,
  index=cover,
  cover_base=1.4,
  cover_bound=level,
  cover_distance=euclidean,
  cover_parallel=auto
);
```

Available keys (all optional):

- `index=auto|cover|brute` – choose the index implementation (`auto` default).
- `dbpath=<file>` – override the SQLite database file used for the shadow table
  lookup and index cache. Use this when the connection has multiple attached
  databases or when the shadow table lives outside the main file. Accepts plain
  paths or `file:` URIs.
- `cover_base=<float>` – override the cover-tree base (>1). Larger bases produce
  shallower trees (faster builds) with looser bounds; smaller bases do the opposite.
- `cover_bound=per_node|level` – pick the pruning strategy (per-node cached radii
  vs level-derived bounds). Default is `per_node`.
- `cover_distance=cosine|euclidean` – set the distance metric used for MATCH queries
  when the cover index rebuilds.
- `cover_parallel=auto|N|off` – parallelize the vector-cloning phase before inserts.
  `auto` uses `GOMAXPROCS`, a positive integer forces that worker count, and `off`
  (or 0) keeps the classic single-threaded build.

Changing any of these settings requires dropping cached blobs from `vector_storage`
so the next query rebuilds with the new parameters. When `index=auto` (default),
`vec` builds a cover tree once a dataset has at least ~4k embedded rows, vectors
of 64+ dimensions, and a document-to-dimension ratio above ~16; smaller or sparse
datasets stay on the brute-force index.

---

## Shadow tables & datasets

Each `vec` virtual table automatically looks for a shadow table named
`_vec_<virtual_table>` (for example, `vec_docs` → `_vec_vec_docs`). The schema
must include:

```sql
dataset_id TEXT NOT NULL,
id         TEXT NOT NULL,
content    TEXT,
meta       TEXT,
embedding  BLOB,
embedding_model TEXT,
scn        INTEGER NOT NULL,
archived   INTEGER NOT NULL DEFAULT 0,
PRIMARY KEY(dataset_id, id)
```

Key rules:

1. `dataset_id` is required on every row and every query. The virtual table
   refuses `MATCH` queries that omit it.
2. Documents/embeddings are immutable per `(dataset_id, id)` pair. Update the
   row only when you intentionally mutate metadata or replace the embedding.
3. `archived = 1` hides a row from higher-level workflows without deleting
   historical data. Reactivating sets it back to 0.
4. Shadow tables should live in the same schema as the virtual table so triggers
   can invalidate `vector_storage` entries automatically.

### Index persistence

`vec` builds a brute-force (or cover-tree) index per dataset slice and persists
it in `vector_storage`. Stored blobs are automatically detected at load time:

- **Legacy** brute-force format (dimension, row count, repeated id+vector data).
- **COV1** cover-tree format (magic header `COV1`, version, dimension, encoded
  tree/values). This is the default when the cover index is used.

Existing brute-force blobs continue to load without changes. Triggers installed
by `vec` delete the `(shadow_table_name, dataset_id)` blob whenever shadow rows
change, forcing the next `MATCH` to rebuild.

---

## Querying with `vec`

```sql
SELECT doc_id, match_score
FROM vec_docs
WHERE dataset_id = 'rootA'
  AND doc_id MATCH ?
  AND match_score >= 0.85   -- optional score filter
LIMIT 20;
```

Notes:

- The virtual table exposes two visible columns (`dataset_id`, `<doc column>`)
  plus the hidden `match_score`.
- The `MATCH` argument is an encoded embedding (`vector.EncodeEmbedding`).
- Results return document IDs; join them back to `shadow_vec_docs` (or a view)
  to fetch `content`/`meta`.
- `vector` also supplies SQL scalar functions (`vec_cosine`, `vec_l2`) for
  ordering/filtering outside the virtual table.

---

## Ingestion & SCN sync

The ingestion pipeline is responsible for:

1. **Discovering datasets** (`vec_dataset`).
2. **Generating documents** (files → chunks → embeddings + metadata).
3. **Writing/updating shadow rows** with deterministic IDs.
4. **Assigning SCNs** and logging every change to `vec_shadow_log`.

See [`docs/INGESTION.md`](docs/INGESTION.md) for a full walkthrough, including:

- Schema expectations (columns, primary keys, indexes).
- Trigger builders (`vecsync.SQLiteShadowLogTriggers`, `vecsync.MySQLShadowLogTriggers`)
  that automatically:
  - allocate SCNs from `vec_dataset_scn`,
  - serialize rows to JSON payloads (hex-encoded embeddings),
  - insert into `vec_shadow_log`.
- Guidance on archiving/reactivation, metadata reuse, and SCN replay ordering.

### Syncing downstream

`vecsync` defines helper structs (`LogEntry`, `SyncState`, `Config`) to build a
replicator:

1. Read the next batch from `vec_shadow_log` for a `(shadow_table, dataset)`.
2. Apply inserts/updates/deletes to the local SQLite shadow table.
3. Update `vec_sync_state` with the latest SCN.
4. Call `vec_invalidate(shadow, dataset)` to flush caches if needed.

A full sync loop lives outside this repo, but the helpers keep schemas and
trigger definitions consistent across deployments.

---

## Administration

- **Cache invalidation:** `vec` automatically creates triggers on every shadow
  table to delete `vector_storage` rows and call `vec_invalidate` whenever data
  changes. No manual steps needed.
- **Manual rebuilds:** `CREATE VIRTUAL TABLE vec_admin USING vec_admin(op)` and run:

  ```sql
  SELECT op FROM vec_admin WHERE op MATCH 'main._vec_vec_docs:rootA';
  -- → "reindexed:<count>"
  ```

- **Storage hygiene:** `vector_storage` stores one blob per `(shadow_table, dataset)`.
  Safe to delete rows manually; `vec` will rebuild on the next query.
- **Monitoring:** Track `vec_sync_state.last_scn` to ensure replicas keep up with
  upstream ingestion.
- **Benchmarking indexes:** Run `go test ./index -bench=.` to compare brute-force
  vs cover-tree builds/queries on your hardware before choosing a default.

### Cover index tuning

`index/cover` now exposes an option-friendly constructor so you can experiment
without editing the module:

- `cover.New(cover.WithBase(1.4))` tweaks the geometric base (fan-out). Larger
  bases create shallower trees (faster builds) at the cost of looser bounds.
- `cover.WithBoundStrategy(cover.BoundLevel)` switches the pruning heuristic
  used at query time (less bookkeeping, slightly lower recall).
- `cover.WithBuildParallelism(n)` parallelizes the vector cloning + magnitude
  phase before sequential inserts. Useful only when copying embeddings dominates
  build time; the tree itself still inserts serially.

Benchmarks capture all of the above. Run:

```bash
env GOPROXY=off GOSUMDB=off GOCACHE=/tmp/sqlite-vec-gocache GOENV=off \
  go test ./index -bench=IndexBuild -run=^$
```

Sample (Apple M1 Max):

```
BenchmarkIndexBuild/cover_50k_128d-10           ~24 ms/op
BenchmarkIndexBuild/cover_parallel_50k_128d-10  ~37 ms/op
```

The parallel variant only accelerates workloads where embedding clones dwarf the
tree insert cost. Keep the default sequential build unless profiles show the
copy step as the bottleneck.

---

## Troubleshooting

| Symptom | Possible cause & fix |
| ------- | -------------------- |
| `SQL logic error: no such module: vec` | Modules were not registered on the same `*sql.DB`, or your SQLite driver lacks the module hook. Call `db.SetMaxOpenConns(1)` before `vec.Register`, ensure you use `engine.Open`, and upgrade/rebase your `modernc.org/sqlite` fork. |
| `vec: dataset_id constraint required` | Every query must include `WHERE dataset_id = ?`. Add the filter even if you only have one dataset. |
| `MATCH` returns 0 rows | Ensure embeddings are encoded via `vector.EncodeEmbedding`, the query vector has the same dimension, and the per-dataset index exists. |
| Index not rebuilding after writes | Verify shadow-table triggers exist (`sqlite_master`), and that the ingestion pipeline updates `dataset_id` consistently. |
| SCN sync stuck | Inspect `vec_shadow_log` for gaps and ensure `vec_sync_state` is updated after each batch. Rebuild from scratch if SCN gaps are unrecoverable. |

---

## Contributing & license

- Issues and PRs welcome—please include repro steps and specify whether you’re
  targeting upstream schemas (MySQL) or local replicas (SQLite).
- This project is licensed under the Apache 2.0 License (see `LICENSE`).
