package core

import (
	"testing"

	"lxs/crypto"
	"lxs/store"
)

// Orphaned branch data (blocks that lost a fork and are now deeper than the
// retention window) is swept off disk. Canonical blocks, genesis, and anything
// within retention are never touched.
func TestOrphanGCDeletesLosingBranchBelowRetention(t *testing.T) {
	old := GCInterval
	GCInterval = 1 // sweep on every prune, for the test
	defer func() { GCInterval = old }()

	k, _ := crypto.GenerateKey()
	gen := testGenesis(k.Address())
	bc, err := NewBlockchain(store.NewMemory(), gen, Options{Retention: 2})
	if err != nil {
		t.Fatal(err)
	}
	genesisBlk := bc.Head()

	// canonical: build to height 2 (genesis state still retained here).
	canon := newBranch(t, bc, genesisBlk)
	c1 := canon.next()
	if err := bc.InsertBlock(c1); err != nil {
		t.Fatal(err)
	}
	if err := bc.InsertBlock(canon.next()); err != nil {
		t.Fatal(err)
	}

	// orphan: a different child of genesis (sibling of c1). Its single-block branch
	// has less total work than the height-2 chain, so it loses and is stored as a
	// side branch. Built now, while genesis state is within retention, since a block
	// on a pruned parent cannot be inserted.
	o1 := newBranch(t, bc, genesisBlk).next()
	if o1.Hash() == c1.Hash() {
		t.Fatal("test setup: orphan equals canonical")
	}
	if err := bc.InsertBlock(o1); err != nil {
		t.Fatal(err)
	}
	if bc.Head().Hash() != canon.tip.Hash() {
		t.Fatal("the lighter orphan must not have become head")
	}
	if !bc.HasBlock(o1.Hash()) {
		t.Fatal("orphan should be stored while still within retention")
	}

	// advance canonical to height 5: cutoff = 5-2 = 3, so the orphan at height 1
	// falls below it and the next sweep must delete it.
	for i := 0; i < 3; i++ {
		if err := bc.InsertBlock(canon.next()); err != nil {
			t.Fatal(err)
		}
	}

	// the orphan is gone from disk; the canonical block at height 1 remains.
	if bc.HasBlock(o1.Hash()) {
		t.Fatal("orphan below retention was NOT garbage-collected")
	}
	if !bc.HasBlock(c1.Hash()) {
		t.Fatal("GC deleted a CANONICAL block — corruption")
	}
	if got, err := bc.BlockByHeight(1); err != nil || got.Hash() != c1.Hash() {
		t.Fatalf("canonical height 1 = %v (err %v), want c1 %s", got, err, c1.Hash().Hex())
	}
	if bc.Head().Height() != 5 {
		t.Fatalf("head = %d, want 5", bc.Head().Height())
	}
	// genesis is never touched.
	if !bc.HasBlock(genesisBlk.Hash()) {
		t.Fatal("GC must never delete genesis")
	}
}
