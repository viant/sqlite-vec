package vector

import "testing"

func TestEncodeDecodeEmbedding_RoundTrip(t *testing.T) {
	orig := []float32{0.0, 1.5, -2.25, 3.75}

	b, err := EncodeEmbedding(orig)
	if err != nil {
		t.Fatalf("EncodeEmbedding failed: %v", err)
	}

	decoded, err := DecodeEmbedding(b)
	if err != nil {
		t.Fatalf("DecodeEmbedding failed: %v", err)
	}
	if len(decoded) != len(orig) {
		t.Fatalf("decoded length = %d, want %d", len(decoded), len(orig))
	}
	for i := range orig {
		if got, want := decoded[i], orig[i]; got != want {
			t.Fatalf("decoded[%d] = %v, want %v", i, got, want)
		}
	}
}

func TestEncodeDecodeEmbedding_Empty(t *testing.T) {
	b, err := EncodeEmbedding(nil)
	if err != nil {
		t.Fatalf("EncodeEmbedding(nil) failed: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("expected empty blob for nil slice, got len=%d", len(b))
	}

	vec, err := DecodeEmbedding(nil)
	if err != nil {
		t.Fatalf("DecodeEmbedding(nil) failed: %v", err)
	}
	if len(vec) != 0 {
		t.Fatalf("expected empty slice for nil blob, got len=%d", len(vec))
	}
}

