package engine

import (
	"math"
	"testing"

	"github.com/viant/sqlite-vec/vector"
)

func TestRegisterVectorFunctionsAndUse(t *testing.T) {
	// Register globally before first connection so functions are available.
	if err := RegisterVectorFunctions(nil); err != nil {
		t.Fatalf("RegisterVectorFunctions failed: %v", err)
	}
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) failed: %v", err)
	}
	defer db.Close()

	if err := RegisterVectorFunctions(db); err != nil {
		t.Fatalf("RegisterVectorFunctions failed: %v", err)
	}

	// Prepare some simple embeddings.
	aBlob, err := vector.EncodeEmbedding([]float32{1, 0})
	if err != nil {
		t.Fatalf("EncodeEmbedding a failed: %v", err)
	}
	bBlob, err := vector.EncodeEmbedding([]float32{0, 1})
	if err != nil {
		t.Fatalf("EncodeEmbedding b failed: %v", err)
	}
	cBlob, err := vector.EncodeEmbedding([]float32{1, 0})
	if err != nil {
		t.Fatalf("EncodeEmbedding c failed: %v", err)
	}

	// vec_cosine orthogonal -> 0
	var sim float64
	if err := db.QueryRow(`SELECT vec_cosine(?, ?)`, aBlob, bBlob).Scan(&sim); err != nil {
		t.Fatalf("vec_cosine(a,b) query failed: %v", err)
	}
	if sim != 0 {
		t.Fatalf("vec_cosine(a,b) = %v, want 0", sim)
	}

	// vec_cosine identical -> 1
	if err := db.QueryRow(`SELECT vec_cosine(?, ?)`, aBlob, cBlob).Scan(&sim); err != nil {
		t.Fatalf("vec_cosine(a,c) query failed: %v", err)
	}
	if math.Abs(sim-1) > 1e-9 {
		t.Fatalf("vec_cosine(a,c) = %v, want 1", sim)
	}

	// vec_l2 between (0,0) and (3,4) -> 5
	zeroBlob, err := vector.EncodeEmbedding([]float32{0, 0})
	if err != nil {
		t.Fatalf("EncodeEmbedding zero failed: %v", err)
	}
	threeFourBlob, err := vector.EncodeEmbedding([]float32{3, 4})
	if err != nil {
		t.Fatalf("EncodeEmbedding threeFour failed: %v", err)
	}

	var dist float64
	if err := db.QueryRow(`SELECT vec_l2(?, ?)`, zeroBlob, threeFourBlob).Scan(&dist); err != nil {
		t.Fatalf("vec_l2 query failed: %v", err)
	}
	if math.Abs(dist-5) > 1e-9 {
		t.Fatalf("vec_l2 = %v, want 5", dist)
	}
}
