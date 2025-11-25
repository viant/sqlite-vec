package engine

import "testing"

// TestOpenInMemory verifies that we can open an in-memory SQLite database
// using the modernc.org/sqlite driver and execute a trivial statement.
func TestOpenInMemory(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) failed: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatalf("CREATE TABLE failed: %v", err)
	}
	if _, err := db.Exec("INSERT INTO t(x) VALUES (1),(2),(3)"); err != nil {
		t.Fatalf("INSERT failed: %v", err)
	}
}

