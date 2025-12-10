// Package vecsync defines foundational types for SCN-based synchronization
// between an upstream SQL database (authoritative shadow tables and
// vec_shadow_log) and downstream SQLite replicas. Higher-level sync
// coordinators will live in this package incrementally; at this stage it
// contains the shared metadata structures used by both the ingestion/log
// producer and the replicator.
package vecsync
