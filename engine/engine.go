package engine

import (
	"database/sql"

	_ "modernc.org/sqlite" // register pure-Go SQLite driver
)

// Open opens a SQLite database using the modernc.org/sqlite driver.
//
// For file-based databases, pass a path like "./db.sqlite". For in-memory
// databases, pass ":memory:".
func Open(dsn string) (*sql.DB, error) { return sql.Open("sqlite", dsn) }
