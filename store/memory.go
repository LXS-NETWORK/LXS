package store

import (
	"bytes"
	"sort"
	"sync"
)

// Memory is an in-memory KV: the implementation every test runs against, and
// what makes a restart testable without a disk (rebuild the Blockchain from the
// same KV, minus the fsync). It does not test crash consistency (whether a
// half-written batch survives a power cut); that is the storage engine's job.
type Memory struct {
	mu     sync.RWMutex
	data   map[string][]byte
	closed bool
}

func NewMemory() *Memory {
	return &Memory{data: make(map[string][]byte)}
}

func (m *Memory) Get(key []byte) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[string(key)]
	if !ok {
		return nil, ErrNotFound
	}
	// Return a copy: handing out the stored slice lets a caller mutate the
	// database by accident.
	return append([]byte(nil), v...), nil
}

func (m *Memory) Has(key []byte) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.data[string(key)]
	return ok, nil
}

func (m *Memory) Put(key, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[string(key)] = append([]byte(nil), value...)
	return nil
}

func (m *Memory) Delete(key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, string(key))
	return nil
}

func (m *Memory) NewBatch() Batch { return &memBatch{db: m} }

func (m *Memory) Iterate(prefix []byte) Iterator {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, 0)
	for k := range m.data {
		if bytes.HasPrefix([]byte(k), prefix) {
			keys = append(keys, k)
		}
	}
	// Sorted: Iterate promises ascending order and the state root depends on it.
	// Go map iteration is randomised.
	sort.Strings(keys)

	values := make([][]byte, len(keys))
	for i, k := range keys {
		values[i] = append([]byte(nil), m.data[k]...)
	}
	return &memIter{keys: keys, values: values, idx: -1}
}

func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// Len reports the number of stored keys. Test-only: proves pruning deletes
// rather than hides.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}

// CountPrefix reports how many keys share a prefix.
func (m *Memory) CountPrefix(prefix []byte) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for k := range m.data {
		if bytes.HasPrefix([]byte(k), prefix) {
			n++
		}
	}
	return n
}

type memOp struct {
	key   []byte
	value []byte
	del   bool
}

type memBatch struct {
	db  *Memory
	ops []memOp
}

func (b *memBatch) Put(key, value []byte) {
	b.ops = append(b.ops, memOp{
		key:   append([]byte(nil), key...),
		value: append([]byte(nil), value...),
	})
}

func (b *memBatch) Delete(key []byte) {
	b.ops = append(b.ops, memOp{key: append([]byte(nil), key...), del: true})
}

func (b *memBatch) Len() int { return len(b.ops) }

// Commit applies every op under a single lock. The lock is the atomicity: no
// reader can observe a half-applied batch.
func (b *memBatch) Commit() error {
	b.db.mu.Lock()
	defer b.db.mu.Unlock()
	for _, op := range b.ops {
		if op.del {
			delete(b.db.data, string(op.key))
			continue
		}
		b.db.data[string(op.key)] = op.value
	}
	b.ops = nil
	return nil
}

func (b *memBatch) Reset() { b.ops = nil }

type memIter struct {
	keys   []string
	values [][]byte
	idx    int
}

func (it *memIter) Next() bool {
	it.idx++
	return it.idx < len(it.keys)
}

func (it *memIter) Key() []byte {
	if it.idx < 0 || it.idx >= len(it.keys) {
		return nil
	}
	return []byte(it.keys[it.idx])
}

func (it *memIter) Value() []byte {
	if it.idx < 0 || it.idx >= len(it.values) {
		return nil
	}
	return it.values[it.idx]
}

func (it *memIter) Error() error { return nil }
func (it *memIter) Close()       {}
