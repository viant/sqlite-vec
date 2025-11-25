// Package vec implements a SQLite virtual table for vector search with
// MATCH semantics. Each virtual table has a per-table shadow table that stores
// ids, content, metadata, and embeddings. A generic index blob is persisted in
// the shared vector_storage table, and an in-memory cache accelerates queries.
//
// Features:
//   - WHERE value MATCH ? using an encoded embedding BLOB
//   - Auto-created shadow tables and triggers
//   - Index persistence in vector_storage and cache invalidation on writes
//   - Pluggable index (brute-force baseline)
package vec

