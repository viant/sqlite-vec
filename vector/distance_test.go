package vector

import "testing"

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	c := []float32{1, 0}

	// Orthogonal vectors -> similarity 0
	if sim, err := CosineSimilarity(a, b); err != nil || sim != 0 {
		t.Fatalf("CosineSimilarity(a,b) = %v, %v; want 0, nil", sim, err)
	}

	// Identical vectors -> similarity 1
	if sim, err := CosineSimilarity(a, c); err != nil || sim != 1 {
		t.Fatalf("CosineSimilarity(a,c) = %v, %v; want 1, nil", sim, err)
	}
}

func TestL2Distance(t *testing.T) {
	a := []float32{0, 0}
	b := []float32{3, 4}

	d, err := L2Distance(a, b)
	if err != nil {
		t.Fatalf("L2Distance failed: %v", err)
	}
	if d != 5 {
		t.Fatalf("L2Distance(0,0)-(3,4) = %v, want 5", d)
	}
}

