package vecsync

import "time"

// LogEntry mirrors a single row in vec_shadow_log on the upstream database.
// It conveys dataset-scoped document changes to downstream replicas.
type LogEntry struct {
	DatasetID   string
	ShadowTable string
	SCN         int64
	Op          string
	DocumentID  string
	Payload     []byte
	CreatedAt   time.Time
}

// SyncState describes the latest SCN applied locally for a given dataset/shadow pair.
// It corresponds to rows in vec_sync_state on downstream SQLite replicas.
type SyncState struct {
	DatasetID   string
	ShadowTable string
	LastSCN     int64
	UpdatedAt   time.Time
}

// Config captures the settings needed to perform SCN-based replication from an
// upstream SQL database into a local SQLite shadow table. Subsequent sync
// components will build on this struct.
type Config struct {
	// DatasetID identifies the dataset slice being synchronized.
	DatasetID string

	// ShadowTable is the fully-qualified shadow table name (e.g., "main.shadow_vec_docs").
	ShadowTable string

	// BatchSize controls how many log entries to fetch/apply per sync iteration.
	BatchSize int
}
