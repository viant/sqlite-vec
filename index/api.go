package index

// Index defines a generic vector index with basic lifecycle methods.
// It enables building from (id, embedding) pairs, kNN queries, and
// binary serialization for persistence.
type Index interface {
    // Build constructs the index from the given ids and vectors.
    // ids and vectors must have the same length; vectors must be non-nil.
    Build(ids []string, vectors [][]float32) error

    // Query runs a kNN search against the index with the provided query vector
    // and returns up to k matches as parallel slices of ids and scores, where
    // higher score means more similar (e.g., cosine similarity).
    Query(query []float32, k int) (ids []string, scores []float64, err error)

    // MarshalBinary serializes the index into a byte slice.
    MarshalBinary() ([]byte, error)

    // UnmarshalBinary reconstructs the index from a serialized byte slice.
    UnmarshalBinary(data []byte) error
}

