package core

import (
	"bytes"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/state"
	"lxs/store"
)

// A deployed contract must survive a restart. Code is folded into the account
// hash, so if loadState drops it the rebuilt root diverges from the head's
// StateRoot and resume() refuses to start — bricking any chain that ever ran
// create-token or the launchpad. This pins both GetCode AND root consistency.
func TestLoadStateRestoresContractCode(t *testing.T) {
	addr := common.Address{0xCC}
	code := []byte{0x60, 0x01, 0x60, 0x00, 0x52, 0x60, 0x01, 0x60, 0x00, 0xf3}

	// Build the world in memory and record the root the head block would commit to.
	s := state.New()
	s.Credit(addr, big.NewInt(5))
	s.SetNonce(addr, 1)
	s.SetCode(addr, code)
	s.SetStorage(addr, common.Hash{0x1}, common.Hash{31: 0x2a})
	wantRoot := s.Root()

	// Persist the account to disk as a committed block does.
	db := store.NewMemory()
	data, err := encodeJSON(s.Get(addr))
	if err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	b.Put(accountKey(addr), data)
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}

	// Restart: rebuild the whole world from disk.
	ls, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := ls.GetCode(addr); !bytes.Equal(got, code) {
		t.Fatalf("contract code lost across restart: got %x, want %x", got, code)
	}
	if ls.Root() != wantRoot {
		t.Fatalf("rebuilt root diverges after restart (%s != %s) — resume() would refuse to start",
			ls.Root().Hex(), wantRoot.Hex())
	}
}

// A reorg that unwinds a block which merely TOUCHED a pre-existing contract must
// restore that contract with its code intact. The reverse diff captures the
// parent's value; dropping Code there leaves the contract codeless on disk, a
// latent split that surfaces as a root mismatch on the next restart.
func TestReverseDiffRestoresContractCode(t *testing.T) {
	db := store.NewMemory()
	addr := common.Address{0xDD}
	code := []byte{0x60, 0x2a, 0x60, 0x00, 0x52}

	// Parent world: the contract already exists with code and balance 100.
	prev := state.New()
	prev.Credit(addr, big.NewInt(100))
	prev.SetCode(addr, code)
	prev.ClearTouched()

	// The block credits the contract (a plain value transfer to it): it is touched,
	// its code unchanged. Forward-write persists balance 200 + code.
	next := prev.Copy()
	next.Credit(addr, big.NewInt(100)) // -> 200

	_, blk := testGenesis().Build() // any block, only its hash is used for the diff key
	b := db.NewBatch()
	if err := writeAccountChanges(b, blk, next, prev, []common.Address{addr}); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}

	// Reorg unwinds the block via its reverse diff.
	b2 := db.NewBatch()
	if err := applyReverseDiff(db, b2, blk.Hash()); err != nil {
		t.Fatal(err)
	}
	if err := b2.Commit(); err != nil {
		t.Fatal(err)
	}

	acc, ok, err := loadAccount(db, addr)
	if err != nil || !ok {
		t.Fatalf("account vanished after reverse diff: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(acc.Code, code) {
		t.Fatalf("reorg unwind dropped contract code: got %x, want %x", acc.Code, code)
	}
	if acc.Balance.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("reorg unwind wrong balance: got %s, want 100", acc.Balance)
	}
}
