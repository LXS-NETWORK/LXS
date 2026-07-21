package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/mempool"
	"lxs/types"
)

// A transaction with a future (gap) nonce is admitted to the pool but is not
// includable, so a built block comes out empty. The node relies on len(blk.Txs)==0
// to decide not to commit, rather than sealing empty blocks while the stuck tx
// keeps the pool non-empty.
func TestBuildSkipsStuckTx(t *testing.T) {
	alice := newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1024)
	prod := NewProducer(bc, pool, common.Address{0x99})

	// alice is at nonce 0; this tx claims nonce 5, a gap that cannot be includable
	// until 0..4 arrive.
	gap := types.NewTransaction(testChainID, 5, common.Address{0x01}, big.NewInt(1), types.IntrinsicGas, big.NewInt(1), nil)
	if err := gap.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(gap, testChainID); err != nil {
		t.Fatalf("pool rejected a future-nonce tx (it should hold it): %v", err)
	}
	if pool.Len() == 0 {
		t.Fatal("the stuck tx is not in the pool")
	}

	blk, err := prod.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(blk.Txs) != 0 {
		t.Fatalf("built block included a stuck nonce-gap tx (%d txs) — the node would flood", len(blk.Txs))
	}

	// The correctly-nonced tx is included; the producer is not refusing everything.
	good := types.NewTransaction(testChainID, 0, common.Address{0x01}, big.NewInt(1), types.IntrinsicGas, big.NewInt(1), nil)
	if err := good.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(good, testChainID); err != nil {
		t.Fatal(err)
	}
	blk2, err := prod.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(blk2.Txs) != 1 {
		t.Fatalf("built block has %d txs, want exactly the 1 includable tx", len(blk2.Txs))
	}
}
