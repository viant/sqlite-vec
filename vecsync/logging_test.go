package vecsync

import (
	"strings"
	"testing"
)

func TestSQLiteShadowLogTriggers(t *testing.T) {
	trigs := SQLiteShadowLogTriggers("main.shadow_vec_docs", "", "")
	if len(trigs) != 3 {
		t.Fatalf("expected 3 triggers, got %d", len(trigs))
	}
	if !strings.Contains(trigs[0], "CREATE TRIGGER IF NOT EXISTS main_shadow_vec_docs_ai AFTER INSERT") {
		t.Fatalf("unexpected insert trigger: %s", trigs[0])
	}
	if !strings.Contains(trigs[1], "'update'") {
		t.Fatalf("update trigger missing op: %s", trigs[1])
	}
	if !strings.Contains(trigs[2], "OLD.dataset_id") {
		t.Fatalf("delete trigger missing OLD reference: %s", trigs[2])
	}
	if !strings.Contains(trigs[0], "lower(hex(NEW.embedding))") {
		t.Fatalf("payload not hex-encoded: %s", trigs[0])
	}
}

func TestMySQLShadowLogTriggers(t *testing.T) {
	trigs := MySQLShadowLogTriggers("shadow_vec_docs", "", "")
	if len(trigs) != 3 {
		t.Fatalf("expected 3 triggers, got %d", len(trigs))
	}
	if !strings.Contains(trigs[0], "CREATE TRIGGER shadow_vec_docs_ai AFTER INSERT") {
		t.Fatalf("unexpected trigger name: %s", trigs[0])
	}
	if !strings.Contains(trigs[0], "JSON_OBJECT(") {
		t.Fatalf("payload must be JSON_OBJECT: %s", trigs[0])
	}
	if !strings.Contains(trigs[0], "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("missing SCN increment: %s", trigs[0])
	}
}
