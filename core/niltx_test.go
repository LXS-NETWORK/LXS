package core

import (
	"testing"
	"time"

	"lxs/mempool"
	"lxs/store"
	"lxs/types"
)

// A single gossiped block carrying a nil transaction ({"txs":[null]} decodes to a
// nil *Transaction) once froze the whole node: VerifyTxRoot -> tx.Hash() nil-derefs
// and panics under bc.mu, whose straight-line Unlock was skipped, poisoning the
// mutex so every later Head()/InsertBlock/RPC read deadlocked. The block reaches that
// code with valid PoW (difficulty/nonce are checked first), so an attacker pays only
// one low-difficulty block to freeze the network. This asserts BOTH properties of the
// fix: InsertBlock rejects it with an error and does NOT panic, and the chain stays
// responsive afterward (mutex released).
func TestNilTxBlockRejectedAndMutexNotPoisoned(t *testing.T) {
	miner := newKey(t)
	g := testGenesis(miner.Address())
	bc, err := NewBlockchain(store.NewMemory(), g, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	prod := NewProducer(bc, mempool.New(1000), miner.Address())

	blk, err := prod.Build() // a real, valid-PoW block (empty body) on top of genesis
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	blk.Txs = []*types.Transaction{nil} // the hostile payload

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("InsertBlock panicked on a nil tx instead of rejecting it: %v", r)
			}
		}()
		if err := bc.InsertBlock(blk); err == nil {
			t.Fatal("a block carrying a nil transaction was accepted")
		}
	}()

	// The mutex must not be poisoned: a read must return promptly, not deadlock.
	done := make(chan uint64, 1)
	go func() { done <- bc.Head().Height() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Head() deadlocked — bc.mu was left locked by the nil-tx insert (poisoned mutex)")
	}
}
