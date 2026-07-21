package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

// The miner receives 100% of the block reward, no split, no deduction.
func TestMinerGetsFullBlockReward(t *testing.T) {
	miner := newKey(t)
	bc := NewMemBlockchain(testGenesis()) // no founder configured

	prod := NewProducer(bc, mempool.New(10), miner.Address())
	if _, err := prod.Seal(); err != nil { // empty block: reward only, no fees
		t.Fatal(err)
	}

	if got := bc.StateSnapshot().Balance(miner.Address()); got.Cmp(state.BaseBlockReward) != 0 {
		t.Fatalf("miner got %s, want 100%% of the reward %s", got, state.BaseBlockReward)
	}
}

// The miner earns block reward plus transaction fees. Fees keep mining worthwhile
// once halving drives the reward toward zero, so this must hold today.
func TestMinerGetsRewardPlusFees(t *testing.T) {
	alice := newKey(t)
	bob := newKey(t)
	miner := newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address())) // alice funded

	pool := mempool.New(10)
	const gasPrice = 5
	if err := pool.Add(signedTxFor(t, alice, 0, bob.Address(), 1000, gasPrice), testChainID); err != nil {
		t.Fatal(err)
	}

	prod := NewProducer(bc, pool, miner.Address())
	blk, err := prod.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if len(blk.Txs) != 1 {
		t.Fatalf("expected the fee-paying tx to be included, got %d txs", len(blk.Txs))
	}

	// fee = gasUsed (IntrinsicGas for a transfer) * gasPrice
	fee := new(big.Int).Mul(big.NewInt(int64(types.IntrinsicGas)), big.NewInt(gasPrice))
	// The miner earns the block reward plus the fee tip (fee minus the burned slice),
	// never the whole fee. The burned slice is destroyed.
	burn := new(big.Int).Div(new(big.Int).Mul(fee, new(big.Int).SetUint64(state.FeeBurnBasisPoints)), big.NewInt(10000))
	tip := new(big.Int).Sub(fee, burn)
	want := new(big.Int).Add(state.BlockRewardAt(blk.Height()), tip)
	if got := bc.StateSnapshot().Balance(miner.Address()); got.Cmp(want) != 0 {
		t.Fatalf("miner balance %s, want reward+tip %s (reward %s + tip %s)",
			got, want, state.BlockRewardAt(blk.Height()), tip)
	}
	if got := bc.StateSnapshot().Burned(); got.Cmp(burn) != 0 {
		t.Fatalf("fee burn %s, want %s (%d bps of fee %s)", got, burn, state.FeeBurnBasisPoints, fee)
	}
}

// The founder's stake is a one-time genesis allocation of 20% of total supply, and
// mining never pays the founder again. The stake is a plain Alloc entry (what
// `init -founder-pct 20` writes), nothing more.
func TestFounderPremineAtGenesisNotPerBlock(t *testing.T) {
	founder := newKey(t)
	holder := newKey(t)
	miner := newKey(t)

	// 200 to the founder + 800 to a holder = 1000 total, founder is 20%.
	g := &Genesis{
		ChainID:   testChainID,
		Timestamp: 1_700_000_000_000,
		GasLimit:  10_000_000,
		Alloc: map[common.Address]*BigStr{
			founder.Address(): {Int: big.NewInt(200)},
			holder.Address():  {Int: big.NewInt(800)},
		},
	}
	bc := NewMemBlockchain(g)

	snap := bc.StateSnapshot()
	premine := snap.Balance(founder.Address())
	total := g.Supply()
	if premine.Cmp(big.NewInt(200)) != 0 {
		t.Fatalf("founder pre-mine: got %s want 200", premine)
	}
	if new(big.Int).Mul(premine, big.NewInt(5)).Cmp(total) != 0 {
		t.Fatalf("founder holds %s of total %s — want exactly 20%%", premine, total)
	}

	// Mine a block with a different miner. The founder balance must not move; the
	// miner takes the whole reward.
	prod := NewProducer(bc, mempool.New(10), miner.Address())
	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}
	snap = bc.StateSnapshot()
	if got := snap.Balance(founder.Address()); got.Cmp(premine) != 0 {
		t.Fatalf("founder balance changed to %s after a block (pre-mine was %s) — mining must never pay the founder", got, premine)
	}
	if got := snap.Balance(miner.Address()); got.Cmp(state.BaseBlockReward) != 0 {
		t.Fatalf("miner got %s, want the full reward %s", got, state.BaseBlockReward)
	}
}
