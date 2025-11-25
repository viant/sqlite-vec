# sqlite-vec - Vector search for SQLite in pure Go
[![GoReportCard](https://goreportcard.com/badge/github.com/viant/sqlite-vec)](https://goreportcard.com/report/github.com/viant/sqlite-vec)
[![GoDoc](https://godoc.org/github.com/viant/sqlite-vec?status.svg)](https://godoc.org/github.com/viant/sqlite-vec)

This library is compatible with Go 1.24+.

- [Motivation](#motivation)
- [Components](#components)
- [Usage](#usage)
- [Higher-level helpers](#higher-level-helpers)
- [SQL helpers](#sql-helpers)
- [Contribution](#contributing-to-sqlite-vec)
- [License](#license)

## Motivation

`sqlite-vec` brings vector search primitives to SQLite using a pure Go stack
based on `modernc.org/sqlite`. It is designed for applications that:

- Want similarity search (kNN) over embeddings without a separate vector DB.
- Prefer a single-file or in-memory SQLite database, including serverless and
  embedded deployments.
- Need a zero-C, CGO-free solution that works well in constrained or
  cross-platform environments.

The project provides:

- A virtual table module (`vec`) with `MATCH` semantics for kNN search over
  stored embeddings.
- A lightweight document model and vector store helpers (`vector`).
- Optional administrative tooling (`vecadmin`) to rebuild and persist indexes.
- SQL scalar functions (`vec_cosine`, `vec_l2`) for ordering and filtering by
  vector similarity directly in SQL.

## Components

- **`engine`**: Thin helpers around `modernc.org/sqlite`:
  - `Open(dsn string)` opens a SQLite database using the pure-Go driver.
  - `RegisterVectorFunctions` wires `vec_cosine` / `vec_l2` into the driver.

- **`vec`**: SQLite virtual table module for vector search:
  - `Register(*sql.DB)` registers a `vec` module on a given database.
  - `CREATE VIRTUAL TABLE ... USING vec(value)` exposes an id-only view backed
    by a per-table shadow table with documents and embeddings.
  - Uses a pluggable index abstraction with a brute-force baseline and a
    future-proof cover-tree adapter.

- **`vector`**: Vector store utilities and encoding:
  - `Document` model and `Store` interface.
  - `SQLiteStore` baseline implementation backed by a `docs` table.
  - `EncodeEmbedding` / `DecodeEmbedding` to pack `[]float32` into SQLite
    `BLOB`s.
  - In-memory `CosineSimilarity` and `L2Distance` helpers.

- **`vecadmin`**: Administrative virtual table for index maintenance:
  - `Register(*sql.DB)` registers a `vec_admin` module.
  - `CREATE VIRTUAL TABLE vec_admin USING vec_admin(op)` exposes a one-column
    control surface.
  - `SELECT op FROM vec_admin WHERE op MATCH '<shadow>'` triggers a rebuild and
    persistence of the index for a given shadow table.

- **`vecutil`**: Optional, embedding-agnostic helpers:
  - `type EmbedFunc func(ctx context.Context, text string) ([]float32, error)`
    is a user-provided function that turns text into embeddings using any
    backend (OpenAI, local model, other APIs).
  - `ShadowTableName(virtualTable string) string` derives the default shadow
    table name for a vec virtual table (for example, `vec_basic` →
    `_vec_vec_basic`).
  - `UpsertShadowDocument` and `UpsertVirtualTableDocument` insert or update
    rows in a shadow table using an `EmbedFunc`.
  - `MatchText` runs a `MATCH` query on a vec virtual table by first embedding
    the query text via `EmbedFunc`.
  - `Index` provides a higher-level, Pinecone-style API with
    `UpsertDocumentsText`, `QueryText`, and `DeleteDocuments` on top of a vec
    virtual table.

## Usage

### 1. Basic setup with pure-Go SQLite

The `engine` package opens a SQLite database using `modernc.org/sqlite` and can
optionally register vector SQL functions globally:

```go
package main

import (
    "log"

    "github.com/viant/sqlite-vec/engine"
)

func main() {
    // Make SQL vector functions (vec_cosine, vec_l2) available to new
    // connections created after this call.
    if err := engine.RegisterVectorFunctions(nil); err != nil {
        log.Fatalf("RegisterVectorFunctions failed: %v", err)
    }

    db, err := engine.Open(":memory:")
    if err != nil {
        log.Fatalf("engine.Open failed: %v", err)
    }
    defer db.Close()

    // For virtual tables it is often convenient to limit to a single
    // underlying connection so module registration is visible everywhere.
    db.SetMaxOpenConns(1)
}
```

### 2. `vec` virtual table: MATCH over embeddings

The `vec` module exposes a virtual table with `MATCH` semantics backed by a
shadow table that stores your documents and embeddings.

Register the module and create a virtual table:

```go
package main

import (
    "log"

    "github.com/viant/sqlite-vec/engine"
    "github.com/viant/sqlite-vec/vec"
)

func main() {
    db, err := engine.Open("./vec_basic.sqlite")
    if err != nil { log.Fatalf("engine.Open failed: %v", err) }
    defer db.Close()

    db.SetMaxOpenConns(1)
    if err := vec.Register(db); err != nil {
        log.Fatalf("vec.Register failed: %v", err)
    }

    if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
        log.Fatalf("PRAGMA setup failed: %v", err)
    }

    if _, err := db.Exec(`CREATE VIRTUAL TABLE vec_basic USING vec(value)`); err != nil {
        log.Fatalf("CREATE VIRTUAL TABLE vec_basic failed: %v", err)
    }

    // The vec_basic virtual table is backed by a shadow table named
    // _vec_vec_basic with the following schema:
    //   id        TEXT PRIMARY KEY
    //   content   TEXT
    //   meta      TEXT
    //   embedding BLOB
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _vec_vec_basic (
        id TEXT PRIMARY KEY,
        content TEXT,
        meta TEXT,
        embedding BLOB
    )`); err != nil {
        log.Fatalf("create shadow failed: %v", err)
    }
}
```

Insert documents into the shadow table and query through the virtual table:

```go
// Insert documents (no embeddings yet).
if _, err := db.Exec(`INSERT INTO _vec_vec_basic(id, content, meta, embedding) VALUES
    ('a1','one','{}',X''),
    ('a2','two','{}',X''),
    ('a3','three','{}',X'')`); err != nil {
    log.Fatalf("insert into shadow failed: %v", err)
}

rows, err := db.Query(`SELECT rowid, value FROM vec_basic ORDER BY rowid`)
if err != nil { log.Fatalf("SELECT failed: %v", err) }
defer rows.Close()

for rows.Next() {
    var rowid int64
    var id string
    if err := rows.Scan(&rowid, &id); err != nil {
        log.Fatalf("scan failed: %v", err)
    }
    log.Printf("row %d → id=%s", rowid, id)
}
```

To use vector search, store embeddings as `BLOB`s and query with `MATCH`:

```go
import "github.com/viant/sqlite-vec/vector"

// Two simple 2D embeddings and a query vector.
e1, _ := vector.EncodeEmbedding([]float32{1, 0})
e2, _ := vector.EncodeEmbedding([]float32{0, 1})
q,  _ := vector.EncodeEmbedding([]float32{1, 0})

// Store embeddings in the shadow table.
if _, err := db.Exec(`INSERT INTO _vec_vec_basic(id, content, meta, embedding) VALUES
    ('d1','one','{}',?),('d2','two','{}',?)`, e1, e2); err != nil {
    log.Fatalf("insert shadow failed: %v", err)
}

// Vector search via MATCH. The MATCH argument is the encoded query embedding.
rows, err := db.Query(`SELECT rowid, value FROM vec_basic WHERE value MATCH ?`, q)
if err != nil { log.Fatalf("SELECT MATCH failed: %v", err) }
defer rows.Close()

for rows.Next() {
    var rowid int64
    var id string
    if err := rows.Scan(&rowid, &id); err != nil {
        log.Fatalf("scan failed: %v", err)
    }
    log.Printf("similar doc → id=%s", id)
}
```

Under the hood the module builds a vector index from the `embedding` column of
the shadow table and answers `MATCH` queries using a brute-force kNN index by
default. Indexes are persisted in a shared `vector_storage` table and cached in
memory for subsequent queries.

### 3. Administrative reindex with `vecadmin`

The `vecadmin` module provides a simple control surface to rebuild persisted
indexes for a given shadow table.

```go
import (
    "github.com/viant/sqlite-vec/vecadmin"
)

// After registering vec and creating virtual/shadow tables:
if err := vecadmin.Register(db); err != nil {
    log.Fatalf("vecadmin.Register failed: %v", err)
}

if _, err := db.Exec(`CREATE VIRTUAL TABLE vec_admin USING vec_admin(op)`); err != nil {
    log.Fatalf("CREATE VIRTUAL TABLE vec_admin failed: %v", err)
}

// Ensure the shared storage table exists for persisted indexes.
if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS vector_storage (
    shadow_table_name TEXT PRIMARY KEY,
    "index" BLOB
)`); err != nil {
    log.Fatalf("create vector_storage failed: %v", err)
}

// Trigger a rebuild for the main._vec_vec_basic shadow table.
rows, err := db.Query(`SELECT op FROM vec_admin WHERE op MATCH 'main._vec_vec_basic'`)
if err != nil { log.Fatalf("vec_admin MATCH failed: %v", err) }
defer rows.Close()

if rows.Next() {
    var op string
    if err := rows.Scan(&op); err != nil {
        log.Fatalf("scan op failed: %v", err)
    }
    log.Printf("admin op result: %s", op) // e.g. "reindexed:2"
}
```

### 4. `vector` package: schema and helpers

The `vector` package defines a simple document model, a `Store` interface, and
helpers for `docs` table schema and embedding encoding.

Example: order by cosine similarity directly in SQL.

```go
package main

import (
    "log"

    "github.com/viant/sqlite-vec/engine"
    "github.com/viant/sqlite-vec/vector"
)

func main() {
    if err := engine.RegisterVectorFunctions(nil); err != nil {
        log.Fatalf("RegisterVectorFunctions failed: %v", err)
    }

    db, err := engine.Open(":memory:")
    if err != nil { log.Fatalf("engine.Open failed: %v", err) }
    defer db.Close()

    if err := vector.EnsureSchema(db); err != nil {
        log.Fatalf("EnsureSchema failed: %v", err)
    }

    e1, _ := vector.EncodeEmbedding([]float32{1, 0})
    e2, _ := vector.EncodeEmbedding([]float32{0, 1})
    q,  _ := vector.EncodeEmbedding([]float32{1, 0})

    if _, err := db.Exec(`INSERT INTO docs(id, content, meta, embedding) VALUES
        ('d1', 'one', '{}', ?),
        ('d2', 'two', '{}', ?)`, e1, e2); err != nil {
        log.Fatalf("insert into docs failed: %v", err)
    }

    rows, err := db.Query(`SELECT id FROM docs ORDER BY vec_cosine(embedding, ?) DESC`, q)
    if err != nil { log.Fatalf("ORDER BY vec_cosine failed: %v", err) }
    defer rows.Close()

    for rows.Next() {
        var id string
        if err := rows.Scan(&id); err != nil {
            log.Fatalf("scan id failed: %v", err)
        }
        log.Printf("ranked id=%s", id)
    }
}
```

The `SQLiteStore` type provides a minimal `Store` implementation over this
schema. It is intentionally conservative and can be extended with richer
indexing and ranking strategies as the project evolves.

## Higher-level helpers

The core packages (`vec`, `vector`, `vecadmin`) are intentionally
embedding-agnostic: they operate on numeric vectors and BLOBs, not on raw
text. The optional `vecutil` package adds a thin layer that “demands” an
embedding function while leaving its implementation up to you.

### `vecutil.EmbedFunc`

`EmbedFunc` is a user-provided function:

```go
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)
```

You decide how this embeds text:

- Call OpenAI or another hosted embedding API.
- Use a local model (for example, sentence transformers).
- Return deterministic or fake embeddings in tests.

The library only requires that you return a `[]float32`; it does not depend on
any specific provider.

### Upserting documents by text

To keep the shadow table in sync with your application data using text
embeddings:

```go
import (
    "context"

    "github.com/viant/sqlite-vec/vecutil"
)

// Example embedding function (implementation is up to you).
var embed vecutil.EmbedFunc = func(ctx context.Context, text string) ([]float32, error) {
    // Call your preferred embedding backend here.
    // For example: OpenAI, a local model, or another API.
    panic("implement me")
}

func upsertDoc(ctx context.Context, db *sql.DB) error {
    // If you know the virtual table name, derive the matching shadow name.
    shadow := vecutil.ShadowTableName("vec_basic") // "_vec_vec_basic"

    return vecutil.UpsertShadowDocument(
        ctx,
        db,
        shadow,
        embed,
        "d1",               // id
        "some content",     // content
        "{}",               // meta
    )
}
```

Or, if you prefer to start from the virtual table name, use the convenience
wrapper:

```go
err := vecutil.UpsertVirtualTableDocument(
    ctx,
    db,
    "vec_basic", // virtual table name
    embed,
    "d1",
    "some content",
    "{}",
)
```

### Text-first MATCH queries

`MatchText` allows you to run a `MATCH` query starting from free-form text:

```go
ids, err := vecutil.MatchText(
    ctx,
    db,
    "vec_basic", // virtual table name
    embed,
    "find similar docs to this", // query text
    10,                           // limit (<=0 means no limit)
)
if err != nil {
    // handle error
}
for _, id := range ids {
    // id corresponds to the id column in the shadow table
}
```

This keeps sqlite-vec embedding-agnostic while giving applications a simple
`func(text) []float32`-style integration point for both indexing and search.

### Index-level API (Pinecone-style)

For a more Pinecone-like experience, `vecutil.Index` wraps a vec virtual table
and its shadow table behind a text-first API. You provide an `EmbedFunc`; the
Index handles embedding, storage, and similarity search.

```go
package main

import (
    "context"
    "database/sql"
    "log"

    "github.com/viant/sqlite-vec/engine"
    "github.com/viant/sqlite-vec/vec"
    "github.com/viant/sqlite-vec/vecutil"
)

// Example embedding function; you decide how to implement this.
var embed vecutil.EmbedFunc = func(ctx context.Context, text string) ([]float32, error) {
    // Call your preferred embedding backend here (OpenAI, local model, etc.).
    // This placeholder must be replaced with a real implementation.
    return nil, fmt.Errorf("embed not implemented")
}

func main() {
    ctx := context.Background()

    db, err := engine.Open("./vec_index.sqlite")
    if err != nil { log.Fatalf("engine.Open failed: %v", err) }
    defer db.Close()
    db.SetMaxOpenConns(1)

    if err := vec.Register(db); err != nil { log.Fatalf("vec.Register failed: %v", err) }

    if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
        log.Fatalf("PRAGMA setup failed: %v", err)
    }

    // Create a vec virtual table and its shadow table.
    if _, err := db.Exec(`CREATE VIRTUAL TABLE vec_docs USING vec(value)`); err != nil {
        log.Fatalf("CREATE VIRTUAL TABLE vec_docs failed: %v", err)
    }
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _vec_vec_docs (
        id TEXT PRIMARY KEY,
        content TEXT,
        meta TEXT,
        embedding BLOB
    )`); err != nil {
        log.Fatalf("create shadow failed: %v", err)
    }

    index, err := vecutil.NewIndex(db, "vec_docs", embed)
    if err != nil { log.Fatalf("NewIndex failed: %v", err) }

    // Upsert documents by text; the Index will embed and store them.
    docs := []vecutil.Document{
        {ID: "d1", Content: "coder is fast", Meta: `{"lang":"en"}`},
        {ID: "d2", Content: "developer writes Go code", Meta: `{"lang":"en"}`},
    }
    if err := index.UpsertDocumentsText(ctx, docs); err != nil {
        log.Fatalf("UpsertDocumentsText failed: %v", err)
    }

    // Query by text; the Index embeds the query and uses vec for kNN.
    matches, err := index.QueryText(ctx, "coder is fast", 10)
    if err != nil {
        log.Fatalf("QueryText failed: %v", err)
    }
    for _, m := range matches {
        log.Printf("match id=%s score=%.4f content=%q meta=%s", m.ID, m.Score, m.Content, m.Meta)
    }
}
```

## SQL helpers

When `RegisterVectorFunctions` has been called, the following SQL functions are
available on connections opened subsequently:

- **`vec_cosine(embedding_a BLOB, embedding_b BLOB)`**: returns cosine
  similarity between two embeddings encoded via `EncodeEmbedding`. Can be used
  in `ORDER BY` or `WHERE` clauses.
- **`vec_l2(embedding_a BLOB, embedding_b BLOB)`**: returns Euclidean (L2)
  distance between two embeddings.

Both functions expect the embeddings as `BLOB`s, typically produced by the
`vector.EncodeEmbedding` helper in Go.

## Contributing to sqlite-vec

`sqlite-vec` is an open source project and contributors are welcome.

Contributions are welcome via issues and pull requests.

If you plan a larger change (new index backend, additional SQL helpers, or
non-trivial schema evolution), please consider opening an issue first to align
on direction and integration points.

## License

The source code is made available under the terms of the Apache License,
Version 2, as stated in the file `LICENSE`.

Individual files may be made available under their own specific license,
all compatible with Apache License, Version 2. Please see individual files for
details.

## Credits and Acknowledgements

**Library Author:** Adrian Witas
