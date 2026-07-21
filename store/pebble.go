//go:build pebble

// Package store — Pebble backend.
//
// Behind a build tag so the default build has zero storage dependencies and
// `go test ./...` runs against Memory in microseconds. To enable:
//
//	go get github.com/cockroachdb/pebble@latest
//	go build -tags pebble ./...
//	go test  -tags pebble ./store/
package store

import (
	"bytes"
	"errors"

	"github.com/cockroachdb/pebble"
)

// Pebble is a KV backed by CockroachDB's Pebble (the engine geth uses). An LSM
// tree: appended, background-merged writes suit a chain's pattern of heavy
// sequential writes and mostly-recent reads.
type Pebble struct {
	db *pebble.DB
	// Sync controls whether every batch is fsynced.
	//   true  = survives a power cut; correct for a real node.
	//   false = faster, survives a process crash but not a power cut. Devnet only.
	Sync bool
}

func OpenPebble(path string) (*Pebble, error) {
	db, err := pebble.Open(path, &pebble.Options{})
	if err != nil {
		return nil, err
	}
	return &Pebble{db: db, Sync: true}, nil
}

func (p *Pebble) writeOpts() *pebble.WriteOptions {
	if p.Sync {
		return pebble.Sync
	}
	return pebble.NoSync
}

func (p *Pebble) Get(key []byte) ([]byte, error) {
	val, closer, err := p.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		// Translate at the boundary: nothing above this package knows the engine.
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	// Pebble's slice is valid only until closer.Close(); copying is mandatory.
	return append([]byte(nil), val...), nil
}

func (p *Pebble) Has(key []byte) (bool, error) {
	_, closer, err := p.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	closer.Close()
	return true, nil
}

func (p *Pebble) Put(key, value []byte) error {
	return p.db.Set(key, value, p.writeOpts())
}

func (p *Pebble) Delete(key []byte) error {
	return p.db.Delete(key, p.writeOpts())
}

func (p *Pebble) NewBatch() Batch {
	return &pebbleBatch{b: p.db.NewBatch(), opts: p.writeOpts()}
}

func (p *Pebble) Iterate(prefix []byte) Iterator {
	it, err := p.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixUpperBound(prefix),
	})
	if err != nil {
		return &pebbleIter{err: err}
	}
	return &pebbleIter{it: it}
}

func (p *Pebble) Close() error { return p.db.Close() }

// prefixUpperBound returns the smallest key greater than every key with the
// prefix, by incrementing the last non-0xff byte. A nil bound means "end of
// keyspace", which would silently return rows from later tables.
func prefixUpperBound(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil // prefix is all 0xff: no upper bound exists
}

type pebbleBatch struct {
	b    *pebble.Batch
	opts *pebble.WriteOptions
	n    int
}

func (b *pebbleBatch) Put(key, value []byte) {
	_ = b.b.Set(key, value, nil)
	b.n++
}

func (b *pebbleBatch) Delete(key []byte) {
	_ = b.b.Delete(key, nil)
	b.n++
}

func (b *pebbleBatch) Len() int { return b.n }

func (b *pebbleBatch) Commit() error {
	if err := b.b.Commit(b.opts); err != nil {
		return err
	}
	b.n = 0
	return b.b.Close()
}

func (b *pebbleBatch) Reset() {
	b.b.Reset()
	b.n = 0
}

type pebbleIter struct {
	it    *pebble.Iterator
	err   error
	first bool
}

func (i *pebbleIter) Next() bool {
	if i.it == nil {
		return false
	}
	if !i.first {
		i.first = true
		return i.it.First()
	}
	return i.it.Next()
}

func (i *pebbleIter) Key() []byte {
	if i.it == nil {
		return nil
	}
	return append([]byte(nil), i.it.Key()...)
}

func (i *pebbleIter) Value() []byte {
	if i.it == nil {
		return nil
	}
	return append([]byte(nil), i.it.Value()...)
}

func (i *pebbleIter) Error() error {
	if i.err != nil {
		return i.err
	}
	if i.it == nil {
		return nil
	}
	return i.it.Error()
}

func (i *pebbleIter) Close() {
	if i.it != nil {
		_ = i.it.Close()
	}
}

var _ = bytes.HasPrefix
