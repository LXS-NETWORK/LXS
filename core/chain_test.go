package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

const testChainID = 1337

func newKey(t *testing.T) *crypto.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testGenesis(funded ...common.Address) *Genesis {
	alloc := make(map[common.Address]*BigStr)
	for _, a := range funded {
		alloc[a] = &BigStr{Int: big.NewInt(1_000_000_000)}
	}
	return &Genesis{
		ChainID:   testChainID,
		Timestamp: 1_700_000_000_000,
		GasLimit:  10_000_000,
		Alloc:     alloc,
	}
}

func TestGenesisIsDeterministic(t *testing.T) {
	a := newKey(t).Address()
	b := newKey(t).Address()
	g := testGenesis(a, b)

	_, blk1 := g.Build()
	_, blk2 := g.Build()
	if blk1.Hash() != blk2.Hash() {
		t.Fatal("genesis hash is not deterministic — nodes would fork at height 0")
	}
}

func TestProduceAndInsertBlock(t *testing.T) {
	alice, bob, miner := newKey(t), newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	tx := types.NewTransaction(testChainID, 0, bob.Address(), big.NewInt(50_000), types.IntrinsicGas, big.NewInt(3), nil)
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(tx, testChainID); err != nil {
		t.Fatal(err)
	}

	blk, err := prod.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if blk.Height() != 1 {
		t.Fatalf("height: got %d want 1", blk.Height())
	}
	if len(blk.Txs) != 1 {
		t.Fatalf("txs: got %d want 1", len(blk.Txs))
	}
	if got := bc.StateSnapshot().Balance(bob.Address()).Int64(); got != 50_000 {
		t.Fatalf("bob balance: got %d want 50000", got)
	}
	if pool.Len() != 0 {
		t.Fatal("mined tx was not evicted from the mempool")
	}
}

// A proposer cannot lie about the resulting state. Here the proposer claims a
// state root that does not match execution; an honest node recomputes and rejects.
func TestForgedStateRootIsRejected(t *testing.T) {
	alice, bob, miner := newKey(t), newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	tx := types.NewTransaction(testChainID, 0, bob.Address(), big.NewInt(1), types.IntrinsicGas, big.NewInt(1), nil)
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	pool.Add(tx, testChainID)

	blk, err := prod.Build()
	if err != nil {
		t.Fatal(err)
	}
	// Proposer forges the state root. Valid difficulty and a real nonce, so it
	// clears the PoW gate and the rejection is about the state root, not the proof.
	forged := &types.Block{
		Header: &types.Header{
			ParentHash: blk.Header.ParentHash,
			Height:     blk.Header.Height,
			Timestamp:  blk.Header.Timestamp,
			TxRoot:     blk.Header.TxRoot,
			StateRoot:  common.Hash{0xde, 0xad, 0xbe, 0xef},
			GasUsed:    blk.Header.GasUsed,
			GasLimit:   blk.Header.GasLimit,
			Proposer:   blk.Header.Proposer,
			Difficulty: blk.Header.Difficulty,
		},
		Txs: blk.Txs,
	}
	mine(forged.Header, nil)
	if err := bc.InsertBlock(forged); err == nil {
		t.Fatal("forged state root accepted")
	}
}

// A header must commit to its body. Swapping the tx list while keeping the header
// is a header/body mismatch attack.
func TestBodySwapIsRejected(t *testing.T) {
	alice, bob, miner := newKey(t), newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	tx := types.NewTransaction(testChainID, 0, bob.Address(), big.NewInt(1), types.IntrinsicGas, big.NewInt(1), nil)
	tx.Sign(alice)
	pool.Add(tx, testChainID)

	blk, _ := prod.Build()

	evil := types.NewTransaction(testChainID, 0, bob.Address(), big.NewInt(999_999), types.IntrinsicGas, big.NewInt(1), nil)
	evil.Sign(alice)
	swapped := &types.Block{Header: blk.Header, Txs: []*types.Transaction{evil}}

	if err := bc.InsertBlock(swapped); err == nil {
		t.Fatal("block body swap accepted")
	}
}

func TestNonSequentialHeightRejected(t *testing.T) {
	alice, miner := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	blk, _ := prod.Build()
	blk.Header.Height = 5
	if err := bc.InsertBlock(blk); err == nil {
		t.Fatal("block with a height gap accepted")
	}
}

// A tx for nonce 1 must not be includable while nonce 0 is missing, or the producer
// builds a block it knows is invalid.
func TestMempoolStopsAtNonceGap(t *testing.T) {
	alice, bob, miner := newKey(t), newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	// Submit nonces 0 and 2, skipping 1.
	for _, n := range []uint64{0, 2} {
		tx := types.NewTransaction(testChainID, n, bob.Address(), big.NewInt(10), types.IntrinsicGas, big.NewInt(1), nil)
		tx.Sign(alice)
		if err := pool.Add(tx, testChainID); err != nil {
			t.Fatal(err)
		}
	}

	blk, err := prod.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if len(blk.Txs) != 1 {
		t.Fatalf("expected only nonce 0 to be executable, got %d txs", len(blk.Txs))
	}
	if blk.Txs[0].Nonce != 0 {
		t.Fatalf("wrong tx included: nonce %d", blk.Txs[0].Nonce)
	}
	if pool.Len() != 1 {
		t.Fatal("the unexecutable tx should still be pending")
	}
}

// Higher gas price should be ordered first across accounts.
func TestGasPriceOrdering(t *testing.T) {
	miner := newKey(t)
	cheap, rich := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(cheap.Address(), rich.Address()))
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	lo := types.NewTransaction(testChainID, 0, miner.Address(), big.NewInt(1), types.IntrinsicGas, big.NewInt(1), nil)
	lo.Sign(cheap)
	hi := types.NewTransaction(testChainID, 0, miner.Address(), big.NewInt(1), types.IntrinsicGas, big.NewInt(100), nil)
	hi.Sign(rich)

	pool.Add(lo, testChainID)
	pool.Add(hi, testChainID)

	blk, err := prod.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(blk.Txs) != 2 {
		t.Fatalf("expected 2 txs, got %d", len(blk.Txs))
	}
	if blk.Txs[0].GasPrice.Int64() != 100 {
		t.Fatal("higher gas price was not ordered first")
	}
}

func TestChainAdvancesOverManyBlocks(t *testing.T) {
	alice, bob, miner := newKey(t), newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	const n = 25
	for i := 0; i < n; i++ {
		tx := types.NewTransaction(testChainID, uint64(i), bob.Address(), big.NewInt(100), types.IntrinsicGas, big.NewInt(1), nil)
		tx.Sign(alice)
		if err := pool.Add(tx, testChainID); err != nil {
			t.Fatal(err)
		}
		if _, err := prod.Seal(); err != nil {
			t.Fatalf("block %d: %v", i+1, err)
		}
	}

	if bc.Head().Height() != n {
		t.Fatalf("head height: got %d want %d", bc.Head().Height(), n)
	}
	if got := bc.StateSnapshot().Balance(bob.Address()).Int64(); got != n*100 {
		t.Fatalf("bob balance: got %d want %d", got, n*100)
	}
	if got := bc.StateSnapshot().Nonce(alice.Address()); got != n {
		t.Fatalf("alice nonce: got %d want %d", got, n)
	}
}

// Supply grows by exactly one block reward per block; fees moving between accounts
// must not change the total. A violation is silent, so this is worth guarding.
func TestSupplyInvariantAcrossBlocks(t *testing.T) {
	alice, bob, miner := newKey(t), newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address(), bob.Address()))
	pool := mempool.New(1000)
	prod := NewProducer(bc, pool, miner.Address())

	supply := func() *big.Int {
		// Total issued supply = circulating (balances) + destroyed (burned). Fee
		// burn means balances alone fall short of issuance by exactly what was
		// burned; adding it back isolates issuance.
		sum := new(big.Int).Set(bc.StateSnapshot().Burned())
		for _, acc := range bc.StateSnapshot().Accounts() {
			sum.Add(sum, acc.Balance)
		}
		return sum
	}
	genesisSupply := supply()

	for i := 0; i < 10; i++ {
		a := types.NewTransaction(testChainID, uint64(i), bob.Address(), big.NewInt(500), types.IntrinsicGas*2, big.NewInt(7), nil)
		a.Sign(alice)
		pool.Add(a, testChainID)
		b := types.NewTransaction(testChainID, uint64(i), alice.Address(), big.NewInt(300), types.IntrinsicGas, big.NewInt(3), nil)
		b.Sign(bob)
		pool.Add(b, testChainID)

		if _, err := prod.Seal(); err != nil {
			t.Fatal(err)
		}
		// Supply grows by exactly one block reward per block; issuance off by a wei
		// is a consensus split, and fee transfers must not change the total.
		want := new(big.Int).Add(genesisSupply, new(big.Int).Mul(big.NewInt(int64(i+1)), state.BaseBlockReward))
		if supply().Cmp(want) != 0 {
			t.Fatalf("issuance at block %d: got %s want %s", i+1, supply(), want)
		}
	}
}
