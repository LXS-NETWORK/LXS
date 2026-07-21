//go:build pebble

package store

// Compile-time proof that the Pebble adapter satisfies the interfaces, so a typo
// fails to build rather than surfacing only on the machine with the dependency.
var (
	_ KV       = (*Pebble)(nil)
	_ Batch    = (*pebbleBatch)(nil)
	_ Iterator = (*pebbleIter)(nil)
)
