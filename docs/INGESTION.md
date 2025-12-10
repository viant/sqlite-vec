# Ingestion & Synchronization Guide

`sqlite-vec` focuses on vector search (`vec` virtual tables, cache persistence,
`match_score`). Feeding data into the shadow tables—and keeping downstream
SQLite replicas in sync—is the responsibility of an external ingestion service.
This document provides a canonical workflow you can adapt to your own system.

---

## 1. Terminology

- **Dataset** – logical collection (filesystem root, tenant, project). Identified
  by `dataset_id`. All tables are partitioned by dataset.
- **Shadow table** – document storage (`shadow_vec_docs` by default). Holds
  document IDs, metadata, embeddings, SCNs, and archive flags.
- **SCN (Sequence Change Number)** – monotonically increasing counter per dataset
  used to order changes. Stored in shadow rows and logged to `vec_shadow_log`.
- **Log table** – `vec_shadow_log`. Upstream DB writes every insert/update/delete
  so replicas can replay deterministically.
- **Sync state** – `vec_sync_state`. Downstream replicas record the last SCN applied.

---

## 2. Bootstrap schemas

Apply:

- `db/schema/mysql/schema.ddl` for the base dataset/shadow/log tables.
- `db/schema/mysql/shadow_sync.ddl` if you want ready-made SCN helpers,
  stored function, and triggers for `shadow_vec_docs`.
- `db/schema/sqlite/schema.ddl` on downstream replicas.

Critical tables:

- `vec_dataset(dataset_id PRIMARY KEY, description, source_uri, last_scn, ...)`
- `shadow_vec_docs(dataset_id, id, content, meta, embedding, embedding_model, scn, archived, timestamps, ...)`
- `vec_shadow_log(dataset_id, shadow_table, scn, op, document_id, payload)`
- `vec_dataset_scn(dataset_id, next_scn)` – helper sequence table.
- `vec_sync_state(dataset_id, shadow_table, last_scn)`
- `vector_storage(shadow_table_name, dataset_id, "index")`

Populate initial datasets:

```sql
INSERT INTO vec_dataset(dataset_id, description, source_uri)
VALUES ('rootA', 'Primary root', 'fs://mount/rootA');
```

---

## 3. Prepare documents

For each file/chunk you want searchable:

1. Extract metadata (logical path, md5, chunk offset, tags, summaries, etc.).
2. Produce `content` – the text that should be embedded.
3. Call your embedding model and capture both the vector and `embedding_model`.
4. Decide on a stable identifier (`id`) per chunk (e.g., `<path>#<chunkIndex>`).
5. Record `archived = 0` for active rows. When retiring, set `archived = 1`.

Document rows are immutable per `(dataset_id, id)`; create a new ID if the
underlying content fundamentally changes.

---

## 4. Upsert into the shadow table

```sql
INSERT INTO shadow_vec_docs (
    dataset_id,
    id,
    content,
    meta,
    embedding,
    embedding_model,
    scn,
    archived
) VALUES (?, ?, ?, ?, ?, ?, ?, 0)
ON DUPLICATE KEY UPDATE
    content = VALUES(content),
    meta = VALUES(meta),
    embedding = VALUES(embedding),
    embedding_model = VALUES(embedding_model),
    scn = VALUES(scn),
    archived = VALUES(archived);
```

Rules:

- `dataset_id` must exist in `vec_dataset`.
- `scn` must reflect the current change (see next section).
- Updates should only overwrite the same `(dataset_id, id)` pair; new content
  versions should use a new ID if you require historical comparisons.

---

## 5. Maintain SCN logs

Every mutation must be replayable downstream. Suggested flow:

1. **Allocate SCN:** increment `vec_dataset_scn.next_scn` for the dataset.
2. **Apply change:** write the row into `shadow_vec_docs`, setting `scn` to the new value.
3. **Log change:** insert into `vec_shadow_log`.

Use the helper trigger builders in `vecsync` to automate steps 1–3:

```go
sql := vecsync.SQLiteShadowLogTriggers("main.shadow_vec_docs", "", "")
for _, stmt := range sql {
    db.Exec(stmt)
}
```

The SQLite helper emits three triggers (AFTER INSERT/UPDATE/DELETE) that:

- Upsert `vec_dataset_scn(dataset_id, next_scn)` with `ON CONFLICT ... DO UPDATE`.
- Serialize the row as JSON (embedding hex-encoded) via `json_object`.
- Insert into `vec_shadow_log`. Primary key: `(dataset_id, shadow_table, scn)`.

For MySQL upstreams, use `vecsync.MySQLShadowLogTriggers`, which emits the same
logic with `JSON_OBJECT`, `ON DUPLICATE KEY UPDATE`, and local variables.

Payload schema (JSON):

```json
{
  "dataset_id": "rootA",
  "id": "doc-001",
  "content": "...",
  "meta": "...",
  "embedding": "hexstring",
  "embedding_model": "model-name",
  "scn": 1234,
  "archived": 0
}
```

---

## 6. Downstream sync loop

Downstream agents (written by you) consume `vec_shadow_log` and update the local
SQLite shadow table:

1. Read the latest checkpoint from `vec_sync_state` for `(dataset_id, shadow_table)`.
2. Fetch the next `N` entries from `vec_shadow_log` ordered by `scn`.
3. Decode each payload, apply insert/update/delete to the local shadow table.
4. Update `vec_sync_state.last_scn`.
5. Call `SELECT vec_invalidate(?, ?)`, passing `shadow_table` and `dataset_id`,
   to drop cached indexes.

This loop can run continuously or via batches. Use `vecsync.LogEntry`,
`vecsync.SyncState`, and `vecsync.Config` as shared data structures across your
agent and tests.

---

## 7. Querying

Once the local shadow table is populated, apps query via the `vec` virtual table:

```sql
SELECT doc_id, match_score
FROM vec_docs
WHERE dataset_id = 'rootA'
  AND doc_id MATCH ?
  AND match_score >= 0.8
ORDER BY match_score DESC
LIMIT 20;
```

Use the returned `doc_id`s to join back to `shadow_vec_docs` (or a view) to
retrieve `content`/`meta`. Always include `dataset_id` in the WHERE clause.

---

## 8. Archiving & reactivation

- Archive: `UPDATE shadow_vec_docs SET archived = 1, scn = nextSCN WHERE dataset_id=? AND id=?;`
- Reactivate: same statement with `archived = 0`.
- Both operations must log to `vec_shadow_log` so downstream replicas follow suit.
- MATCH queries should default to `archived = 0` (enforced in your application or
  via a view that joins the virtual table to the shadow table).

---

## 9. Administration & maintenance

- **Index persistence:** `vec` stores indexes in `vector_storage`. Triggers delete
  entries automatically when shadow rows change.
- **Manual rebuilds:** run `SELECT op FROM vec_admin WHERE op MATCH 'shadow:dataset';`
  after bulk loads or when upgrading index implementations.
- **Retention:** prune `vec_shadow_log` once all replicas have advanced past a given
  SCN (check `vec_sync_state`). Retain enough history for disaster recovery.
- **Backfills:** when backfilling large datasets, it’s often faster to bulk load the
  shadow table, truncate the matching `vector_storage` rows, and let `vec_admin` rebuild.

---

## 10. Checklist

1. [ ] Apply schemas (`db/schema/*/schema.ddl`).
2. [ ] Populate `vec_dataset`.
3. [ ] Install SCN triggers via `vecsync` helper functions.
4. [ ] Build ingestion job (files → embeddings → shadow inserts).
5. [ ] Run downstream sync loop (consume `vec_shadow_log`).
6. [ ] Register `vec` / `vec_admin` modules in SQLite.
7. [ ] Query via `MATCH`, rebuild indexes as needed.

With this workflow, you can keep upstream ingestion logic in any language/stack
while leveraging `sqlite-vec` for local, query-friendly vector search.
