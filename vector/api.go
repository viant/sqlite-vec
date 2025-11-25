package vector

import (
	"context"
)

// Document represents a logical document stored in the vector store. It is a
// simplified placeholder that can be refined as the integration with
// Embedius/schema progresses.
type Document struct {
	// ID is the logical identifier of the document. When empty on insert, the
	// store may generate one.
	ID string

	// Content holds the main text/body of the document.
	Content string

	// Metadata is an opaque JSON or structured payload associated with the
	// document. For now it is modeled as a raw string to avoid a dependency on a
	// particular JSON library.
	Metadata string

	// Embedding is the vector representation of the document content.
	Embedding []float32
}

// Store defines the application-level vector store API. Implementations in
// this module will use SQLite for durable storage and a pure-Go index (e.g.
// Embedius+GDS) for kNN search.
type Store interface {
	// AddDocuments inserts documents into the store and returns their assigned
	// IDs. If a Document has ID set, implementations should attempt to honor it
	// (subject to uniqueness constraints).
	AddDocuments(ctx context.Context, docs []Document) ([]string, error)

	// SimilaritySearch performs a k-nearest-neighbour search using the provided
	// embedding as the query vector and returns up to k matching documents.
	SimilaritySearch(ctx context.Context, queryEmbedding []float32, k int) ([]Document, error)

	// Remove deletes or tombstones the document with the given ID from both the
	// underlying SQLite tables and the vector index.
	Remove(ctx context.Context, id string) error
}

