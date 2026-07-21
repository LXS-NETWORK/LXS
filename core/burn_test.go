package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/mempool"
	"lxs/state"
	"lxs/store"
	"lxs/types"
)

// Mines a real burn, closes the chain, and reopens it from the same database. The
// reopen is the strongest assertion: resume() re-verifies the head's state root,
// which commits to the burn total, so an unpersisted total would fail the root
// check and NewBlockchain would refuse to open.
func TestBurnPersistsAcrossRestart(t *testing.T) {
	alice, miner := newKey(t), newKey(t)
	g := testGenesis(alice.Address()) // funds alice 1e9

	db := store.NewMemory()
	bc, err := NewBlockchain(db, g, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	burnAmt := big.NewInt(400_000)
	tx := types.NewTransaction(testChainID, 0, common.BurnAddress, burnAmt, types.IntrinsicGas, big.NewInt(1), nil)
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(tx, testChainID); err != nil {
		t.Fatal(err)
	}
	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}

	// The sent value plus the fee burn of this tx's own fee (gasPrice 1).
	feeBurn := new(big.Int).Div(
		new(big.Int).Mul(big.NewInt(int64(types.IntrinsicGas)), new(big.Int).SetUint64(state.FeeBurnBasisPoints)),
		big.NewInt(10000))
	wantBurned := new(big.Int).Add(burnAmt, feeBurn)
	if got := bc.StateSnapshot().Burned(); got.Cmp(wantBurned) != 0 {
		t.Fatalf("burned before restart = %s, want %s", got, wantBurned)
	}
	if got := bc.StateSnapshot().Balance(common.BurnAddress); got.Sign() != 0 {
		t.Fatalf("burn address balance = %s, want 0", got)
	}
	wantRoot := bc.StateSnapshot().Root()
	if err := bc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen from disk; if burned did not persist, resume()'s root check fails here.
	bc2, err := NewBlockchain(db, g, Options{})
	if err != nil {
		t.Fatalf("reopen (burn total likely not persisted, so the head root check failed): %v", err)
	}
	if got := bc2.StateSnapshot().Burned(); got.Cmp(wantBurned) != 0 {
		t.Fatalf("burned after restart = %s, want %s", got, wantBurned)
	}
	if got := bc2.StateSnapshot().Root(); got != wantRoot {
		t.Fatalf("reconstructed root %s != %s", got.Hex(), wantRoot.Hex())
	}
}
