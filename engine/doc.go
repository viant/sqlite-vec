// Package engine provides helpers for working with the modernc.org/sqlite
// driver in this module: opening connections and (optionally) registering
// SQL scalar functions. It intentionally keeps a thin surface so other
// packages can share the same driver instance.
package engine
