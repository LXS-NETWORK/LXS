package core

import (
	"testing"

	"math/big"

	"lxs/common"
	"lxs/mempool"
	"lxs/store"
	"lxs/types"
)

// After a restart, the in-memory reorg window must be rebuilt from reverse diffs, so
// a restarted node can still follow a fork that diverged within retention. This mines
// a chain (with burns, to exercise BurnDelta), reopens it from the same store, and
// asserts an ancestor's state is present AND roots correctly — the exact capability a
// head-only resume threw away.
func TestRestartRebuildsReorgWindow(t *testing.T) {
	db := store.NewMemory()
	alice, miner := newKey(t), newKey(t)
	g := testGenesis(alice.Address())

	bc, err := NewBlockchain(db, g, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pool := mempool.New(100)
	prod := NewProducer(bc, pool, miner.Address())

	nonce := uint64(0)
	for i := 0; i < 10; i++ {
		if i%3 == 0 { // a burn tx every few blocks -> non-zero BurnDelta to reconstruct
			tx := types.NewTransaction(testChainID, nonce, common.BurnAddress, big.NewInt(1000), types.IntrinsicGas, big.NewInt(2), nil)
			if err := tx.Sign(alice); err != nil {
				t.Fatal(err)
			}
			if err := pool.Add(tx, testChainID); err != nil {
				t.Fatal(err)
			}
			nonce++
		}
		blk, err := prod.Build()
		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		if err := prod.Commit(blk); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	ancestor, err := bc.BlockByHeight(bc.Head().Height() - 5)
	if err != nil {
		t.Fatal(err)
	}

	// Reopen from the SAME store: resume() runs rebuildWindow.
	bc2, err := NewBlockchain(db, g, Options{})
	if err != nil {
		t.Fatalf("reopen failed (a brick): %v", err)
	}
	st, err := bc2.StateAt(ancestor.Hash())
	if err != nil {
		t.Fatalf("ancestor state not rebuilt after restart: %v (reorg window collapsed to zero)", err)
	}
	if st.Root() != ancestor.Header.StateRoot {
		t.Fatalf("rebuilt ancestor state roots to %s, block says %s", st.Root().Hex(), ancestor.Header.StateRoot.Hex())
	}
}
