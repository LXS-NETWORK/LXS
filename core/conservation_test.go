package core

import (
	"math/big"
	"testing"

	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

// Value conservation must hold across mining, transfers, and burns:
// Σ balances + burned == genesisSupply + issued, at every height.
func TestConservationHoldsAcrossChain(t *testing.T) {
	miner, alice := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(100)
	prod := NewProducer(bc, pool, miner.Address())

	for i := 0; i < 5; i++ {
		if i == 2 {
			tx := types.NewTransaction(testChainID, 0, miner.Address(), big.NewInt(1000), types.IntrinsicGas, big.NewInt(1), nil)
			if err := tx.Sign(alice); err != nil {
				t.Fatal(err)
			}
			if err := pool.Add(tx, testChainID); err != nil {
				t.Fatal(err)
			}
		}
		blk, err := prod.Build()
		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		if err := prod.Commit(blk); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if err := bc.CheckConservation(); err != nil {
		t.Fatalf("conservation must hold: %v", err)
	}
	if state.CumulativeIssued(bc.Head().Height()).Sign() <= 0 {
		t.Fatal("CumulativeIssued should be positive after mining")
	}
}
