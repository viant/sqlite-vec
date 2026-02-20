package engine

import (
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"math"

	sqlite "modernc.org/sqlite"
)

// RegisterVectorFunctions registers vec_cosine and vec_l2 with the driver so
// they are available on new connections opened after this call.
// Note: existing open connections will not see new functions.
func RegisterVectorFunctions(_ *sql.DB) error {
	// Idempotent registration; driver rejects duplicates but we ignore errors silently here.
	_ = sqlite.RegisterDeterministicScalarFunction("vec_cosine", 2, vecCosineImpl)
	_ = sqlite.RegisterDeterministicScalarFunction("vec_l2", 2, vecL2Impl)
	return nil
}

func asEmbedding(arg driver.Value) ([]float32, error) {
	switch v := arg.(type) {
	case nil:
		return nil, nil
	case []byte:
		return decodeEmbedding(v)
	default:
		return nil, fmt.Errorf("vec: unsupported argument type %T for embedding; want BLOB", arg)
	}
}

func vecCosineImpl(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("vec_cosine: expected 2 arguments, got %d", len(args))
	}
	a, err := asEmbedding(args[0])
	if err != nil {
		return nil, err
	}
	b, err := asEmbedding(args[1])
	if err != nil {
		return nil, err
	}
	if a == nil || b == nil {
		return nil, nil
	}
	sim, err := cosine(a, b)
	if err != nil {
		return nil, err
	}
	return sim, nil
}

func vecL2Impl(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("vec_l2: expected 2 arguments, got %d", len(args))
	}
	a, err := asEmbedding(args[0])
	if err != nil {
		return nil, err
	}
	b, err := asEmbedding(args[1])
	if err != nil {
		return nil, err
	}
	if a == nil || b == nil {
		return nil, nil
	}
	d, err := l2(a, b)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// Local minimal helpers to avoid import cycles in tests.
func decodeEmbedding(b []byte) ([]float32, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("vec: invalid embedding blob length %d", len(b))
	}
	n := len(b) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

func cosine(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("vec: cosine dim mismatch %d vs %d", len(a), len(b))
	}
	if len(a) == 0 {
		return 0, fmt.Errorf("vec: cosine on empty vectors")
	}
	var dot, na2, nb2 float64
	for i := range a {
		va := float64(a[i])
		vb := float64(b[i])
		dot += va * vb
		na2 += va * va
		nb2 += vb * vb
	}
	if na2 == 0 || nb2 == 0 {
		return 0, fmt.Errorf("vec: cosine with zero-magnitude vector")
	}
	return dot / (math.Sqrt(na2) * math.Sqrt(nb2)), nil
}

func l2(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("vec: L2 dim mismatch %d vs %d", len(a), len(b))
	}
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return math.Sqrt(sum), nil
}
