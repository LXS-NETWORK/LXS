package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/mempool"
	"lxs/state"
)

// resetSplit clears the consensus reward-split globals after a test so they do
// not leak into the next (NewBlockchain sets them from each genesis, but a test
// that does not open a fresh chain afterwards would otherwise inherit them).
func resetSplit(t *testing.T) {
	t.Cleanup(func() { state.TreasuryRewardBasisPoints, state.TreasuryRewardAddress = 0, common.Address{} })
}

// With the reward split configured in genesis, a sealed and validated block lands
// 80% with the proposer and 20% with the treasury. NewBlockchain loads the split
// from genesis, so producer and validator share it; a mismatch would make the state
// root reject the very block that was produced.
func TestBlockRewardSplitFlowsThroughProduction(t *testing.T) {
	resetSplit(t)
	miner := newKey(t)
	treasury := common.Address{0x42}

	g := testGenesis()
	g.TreasuryReward = treasury
	g.TreasuryRewardBps = 2000

	bc := NewMemBlockchain(g)
	prod := NewProducer(bc, mempool.New(10), miner.Address())
	blk, err := prod.Seal()
	if err != nil {
		t.Fatal(err)
	}

	reward := state.BlockRewardAt(blk.Height())
	wantTreasury := new(big.Int).Div(new(big.Int).Mul(reward, big.NewInt(2000)), big.NewInt(10000))
	wantMiner := new(big.Int).Sub(reward, wantTreasury)

	snap := bc.StateSnapshot()
	if got := snap.Balance(treasury); got.Cmp(wantTreasury) != 0 {
		t.Fatalf("treasury got %s through a produced block, want 20%% = %s", got, wantTreasury)
	}
	if got := snap.Balance(miner.Address()); got.Cmp(wantMiner) != 0 {
		t.Fatalf("proposer got %s, want 80%% = %s", got, wantMiner)
	}
	if sum := new(big.Int).Add(snap.Balance(treasury), snap.Balance(miner.Address())); sum.Cmp(reward) != 0 {
		t.Fatalf("proposer+treasury = %s, want exactly the block reward %s", sum, reward)
	}
}

// With the treasury target set to the burn address in genesis, the reward cut is
// destroyed: it folds into the consensus burn total, no address holds it, and the
// proposer keeps the rest. Trustless deflation, no key or treasury needed.
func TestBlockRewardAutoBurnThroughProduction(t *testing.T) {
	resetSplit(t)
	miner := newKey(t)

	g := testGenesis()
	g.TreasuryReward = common.BurnAddress
	g.TreasuryRewardBps = 2000

	bc := NewMemBlockchain(g)
	burnedBefore := bc.StateSnapshot().Burned()

	prod := NewProducer(bc, mempool.New(10), miner.Address())
	blk, err := prod.Seal() // empty block: reward only, so burn is purely the reward cut
	if err != nil {
		t.Fatal(err)
	}

	reward := state.BlockRewardAt(blk.Height())
	wantBurn := new(big.Int).Div(new(big.Int).Mul(reward, big.NewInt(2000)), big.NewInt(10000))
	wantMiner := new(big.Int).Sub(reward, wantBurn)

	snap := bc.StateSnapshot()
	gotBurn := new(big.Int).Sub(snap.Burned(), burnedBefore)
	if gotBurn.Cmp(wantBurn) != 0 {
		t.Fatalf("auto-burn = %s, want 20%% of the reward = %s", gotBurn, wantBurn)
	}
	// Destroyed, not parked: the burn address must hold nothing.
	if got := snap.Balance(common.BurnAddress); got.Sign() != 0 {
		t.Fatalf("burn address holds %s — an auto-burn must destroy, not credit", got)
	}
	if got := snap.Balance(miner.Address()); got.Cmp(wantMiner) != 0 {
		t.Fatalf("proposer got %s, want 80%% = %s", got, wantMiner)
	}
	// conservation: what the miner holds + what was burned == the full reward.
	if sum := new(big.Int).Add(wantMiner, gotBurn); sum.Cmp(reward) != 0 {
		t.Fatalf("miner + burned = %s, want the full issued reward %s", sum, reward)
	}
}

// TestGenesisTreasuryValidation pins the both-or-nothing rule and the ceiling.
func TestGenesisTreasuryValidation(t *testing.T) {
	base := func() *Genesis { return testGenesis() }

	overCap := base()
	overCap.TreasuryReward = common.Address{0x1}
	overCap.TreasuryRewardBps = 10001
	if overCap.Validate() == nil {
		t.Fatal("treasuryRewardBps > 10000 must be rejected")
	}

	bpsNoAddr := base()
	bpsNoAddr.TreasuryRewardBps = 2000 // no address
	if bpsNoAddr.Validate() == nil {
		t.Fatal("a bps with no treasury address must be rejected (would pay the zero address)")
	}

	addrNoBps := base()
	addrNoBps.TreasuryReward = common.Address{0x1} // no bps
	if addrNoBps.Validate() == nil {
		t.Fatal("a treasury address with no bps must be rejected")
	}

	ok := base()
	ok.TreasuryReward = common.BurnAddress
	ok.TreasuryRewardBps = 2000
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid both-set treasury config was rejected: %v", err)
	}
}
