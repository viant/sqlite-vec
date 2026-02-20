package vector

import (
	"encoding/binary"
	"fmt"
	"math"
)

// EncodeEmbedding encodes a slice of float32 values into a BLOB representation
// suitable for storage in SQLite. The current encoding is a simple
// little-endian sequence of IEEE 754 float32 values without a length prefix;
// the length is derived from the BLOB size on decode.
func EncodeEmbedding(vec []float32) ([]byte, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	b := make([]byte, len(vec)*4)
	for i, v := range vec {
		bits := math.Float32bits(v)
		binary.LittleEndian.PutUint32(b[i*4:], bits)
	}
	return b, nil
}

// DecodeEmbedding decodes a BLOB produced by EncodeEmbedding back into a
// slice of float32 values.
func DecodeEmbedding(b []byte) ([]float32, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("vector: invalid embedding blob length %d (not multiple of 4)", len(b))
	}
	n := len(b) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		bits := binary.LittleEndian.Uint32(b[i*4:])
		vec[i] = math.Float32frombits(bits)
	}
	return vec, nil
}
