package store

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestGetPutDelete(t *testing.T) {
	db := NewMemory()

	if _, err := db.Get([]byte("nope")); err != ErrNotFound {
		t.Fatalf("missing key: got %v want ErrNotFound", err)
	}
	if err := db.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	got, err := db.Get([]byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v" {
		t.Fatalf("got %q want %q", got, "v")
	}
	if ok, _ := db.Has([]byte("k")); !ok {
		t.Fatal("Has says no for a key that exists")
	}
	if err := db.Delete([]byte("k")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.Has([]byte("k")); ok {
		t.Fatal("Has says yes for a deleted key")
	}
}

// Handing out the internal slice lets a caller mutate the database by accident.
func TestGetReturnsACopy(t *testing.T) {
	db := NewMemory()
	db.Put([]byte("k"), []byte("original"))

	got, _ := db.Get([]byte("k"))
	got[0] = 'X'

	again, _ := db.Get([]byte("k"))
	if string(again) != "original" {
		t.Fatalf("mutating a returned slice changed the database: %q", again)
	}
}

func TestPutCopiesTheValue(t *testing.T) {
	db := NewMemory()
	v := []byte("original")
	db.Put([]byte("k"), v)
	v[0] = 'X' // caller reuses its buffer, as callers do

	got, _ := db.Get([]byte("k"))
	if string(got) != "original" {
		t.Fatalf("the store aliased the caller's buffer: %q", got)
	}
}

// The property the chain rests on: a batch is all or nothing, never seen half-applied.
func TestBatchIsNotVisibleUntilCommit(t *testing.T) {
	db := NewMemory()
	db.Put([]byte("existing"), []byte("old"))

	b := db.NewBatch()
	b.Put([]byte("new"), []byte("value"))
	b.Put([]byte("existing"), []byte("updated"))
	b.Delete([]byte("existing"))

	if ok, _ := db.Has([]byte("new")); ok {
		t.Fatal("an uncommitted batch write is visible")
	}
	got, _ := db.Get([]byte("existing"))
	if string(got) != "old" {
		t.Fatal("an uncommitted batch changed an existing key")
	}
	if b.Len() != 3 {
		t.Fatalf("batch length: got %d want 3", b.Len())
	}

	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.Get([]byte("new")); string(v) != "value" {
		t.Fatal("commit did not apply the put")
	}
	if ok, _ := db.Has([]byte("existing")); ok {
		t.Fatal("commit did not apply the delete")
	}
}

// Operations must apply in the order they were queued. Out of order, a
// Put-then-Delete becomes a Delete-then-Put and the key survives.
func TestBatchAppliesInOrder(t *testing.T) {
	db := NewMemory()
	b := db.NewBatch()
	b.Put([]byte("k"), []byte("first"))
	b.Put([]byte("k"), []byte("second"))
	b.Commit()

	got, _ := db.Get([]byte("k"))
	if string(got) != "second" {
		t.Fatalf("batch ordering is wrong: got %q want %q", got, "second")
	}

	b2 := db.NewBatch()
	b2.Put([]byte("x"), []byte("v"))
	b2.Delete([]byte("x"))
	b2.Commit()
	if ok, _ := db.Has([]byte("x")); ok {
		t.Fatal("put-then-delete left the key behind")
	}
}

func TestBatchReset(t *testing.T) {
	db := NewMemory()
	b := db.NewBatch()
	b.Put([]byte("k"), []byte("v"))
	b.Reset()
	if b.Len() != 0 {
		t.Fatal("reset did not clear the batch")
	}
	b.Commit()
	if ok, _ := db.Has([]byte("k")); ok {
		t.Fatal("a reset batch still wrote")
	}
}

// Iterate promises ascending key order; the state root depends on it. An
// unsorted walk would produce a different root each run.
func TestIterateIsSortedAndPrefixScoped(t *testing.T) {
	db := NewMemory()
	// Inserted in deliberately jumbled order.
	for _, k := range []string{"a9", "a1", "b5", "a3", "z0", "a0"} {
		db.Put([]byte(k), []byte("v"+k))
	}

	it := db.Iterate([]byte("a"))
	defer it.Close()

	var got []string
	for it.Next() {
		got = append(got, string(it.Key()))
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}

	want := []string{"a0", "a1", "a3", "a9"}
	if len(got) != len(want) {
		t.Fatalf("prefix scan returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("iteration order: got %v want %v", got, want)
		}
	}
}

func TestIterateEmptyPrefix(t *testing.T) {
	db := NewMemory()
	it := db.Iterate([]byte("missing"))
	defer it.Close()
	if it.Next() {
		t.Fatal("a prefix with no matches yielded a key")
	}
}

func TestIterateValues(t *testing.T) {
	db := NewMemory()
	db.Put([]byte("p1"), []byte("one"))
	db.Put([]byte("p2"), []byte("two"))

	it := db.Iterate([]byte("p"))
	defer it.Close()

	seen := map[string]string{}
	for it.Next() {
		seen[string(it.Key())] = string(it.Value())
	}
	if seen["p1"] != "one" || seen["p2"] != "two" {
		t.Fatalf("iterator values are wrong: %v", seen)
	}
}

// The store is touched from the RPC goroutine, the block producer and every peer
// connection; it must be safe under all of them.
func TestConcurrentAccess(t *testing.T) {
	db := NewMemory()
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				key := []byte(fmt.Sprintf("k%d-%d", n, j))
				db.Put(key, []byte("v"))
				db.Get(key)
				db.Has(key)

				b := db.NewBatch()
				b.Put([]byte(fmt.Sprintf("b%d-%d", n, j)), []byte("v"))
				b.Commit()

				it := db.Iterate([]byte("k"))
				for it.Next() {
				}
				it.Close()
			}
		}(i)
	}
	wg.Wait()

	if db.Len() != 8*100*2 {
		t.Fatalf("keys: got %d want %d", db.Len(), 8*100*2)
	}
}

// A committed batch must be observed as one unit, never some keys applied and
// others not.
func TestBatchAtomicityUnderConcurrentReads(t *testing.T) {
	db := NewMemory()
	keys := [][]byte{[]byte("x1"), []byte("x2"), []byte("x3")}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			// Either all three exist or none; anything else is a half-applied
			// batch. Read via CountPrefix (one RLock): three separate Has()
			// calls are independent critical sections, so an atomic Commit
			// landing between them would report a false partial. This still
			// attacks the code: if Commit released the lock mid-batch,
			// CountPrefix would see 1 or 2 and panic.
			n := db.CountPrefix([]byte("x"))
			if n != 0 && n != len(keys) {
				panic(fmt.Sprintf("observed a partially applied batch: %d of %d keys", n, len(keys)))
			}
		}
	}()

	for i := 0; i < 500; i++ {
		b := db.NewBatch()
		for _, k := range keys {
			b.Put(k, []byte("v"))
		}
		b.Commit()

		b2 := db.NewBatch()
		for _, k := range keys {
			b2.Delete(k)
		}
		b2.Commit()
	}
	close(done)
	wg.Wait()
}

func TestCountPrefix(t *testing.T) {
	db := NewMemory()
	db.Put([]byte("aa"), []byte("1"))
	db.Put([]byte("ab"), []byte("2"))
	db.Put([]byte("ba"), []byte("3"))

	if n := db.CountPrefix([]byte("a")); n != 2 {
		t.Fatalf("CountPrefix: got %d want 2", n)
	}
	if n := db.CountPrefix([]byte("z")); n != 0 {
		t.Fatalf("CountPrefix for a missing prefix: got %d want 0", n)
	}
}

var _ = bytes.HasPrefix
