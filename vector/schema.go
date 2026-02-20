package vector

import (
	"database/sql"
)

const docsSchema = `
CREATE TABLE IF NOT EXISTS docs (
    id TEXT PRIMARY KEY,
    content TEXT,
    meta TEXT,
    embedding BLOB
);
`

// EnsureSchema creates the base documents table in the provided database if it
// does not already exist. The initial schema is intentionally simple and can
// be evolved as the integration with Embedius/schema.Document matures.
func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(docsSchema)
	return err
}
