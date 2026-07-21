package store

import "errors"

// ErrNotFound is returned by Get for a key that does not exist.
// It is a normal outcome, not a failure.
var ErrNotFound = errors.New("store: not found")

// KV is the storage contract the chain depends on. An interface, not Pebble
// directly: a chain hard-wired to one database cannot be tested without it.
// Every test runs against Memory in microseconds (cf. geth's ethdb.Database).
//
// Implementations must be safe for concurrent use.
type KV interface {
	// Get returns ErrNotFound if the key is absent.
	Get(key []byte) ([]byte, error)
	Has(key []byte) (bool, error)
	Put(key, value []byte) error
	Delete(key []byte) error

	// NewBatch returns a write batch. See Batch.
	NewBatch() Batch

	// Iterate walks all keys with the given prefix, in ascending key order.
	// Ordering is part of the contract: the state root depends on it.
	Iterate(prefix []byte) Iterator

	Close() error
}

// Batch is a set of writes that commit atomically, all or nothing. Writing a
// block writes the block, receipts, tx index, height mapping, account changes
// and the head pointer; a crash mid-list must leave the node on the old head
// with nothing half-applied.
//
// The head pointer must land in the same batch as what it points at. Head first
// and a crash names a block that does not exist; head last and unbatched, a
// crash leaves a block the node never notices.
type Batch interface {
	Put(key, value []byte)
	Delete(key []byte)
	// Len is the number of queued operations.
	Len() int
	// Commit applies every queued operation atomically.
	Commit() error
	Reset()
}

// Iterator walks a key range. Always Close it.
type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Error() error
	Close()
}
