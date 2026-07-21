package state

import (
	"math/big"
	"testing"

	"lxs/common"
)

// CreditBlockReward routes a configured share of the block reward to a treasury
// address, the rest to the proposer. Default is off (100% proposer); these tests
// set and reset the package dials to cover both paths.

// withSplit sets the reward-split dials for the duration of a test and restores
// them after — the dials are package-global consensus constants, so leaking a
// non-default value would silently change every later test's reward math.
func withSplit(t *testing.T, bps int64, addr common.Address) {
	t.Helper()
	obps, oaddr := TreasuryRewardBasisPoints, TreasuryRewardAddress
	TreasuryRewardBasisPoints, TreasuryRewardAddress = bps, addr
	t.Cleanup(func() { TreasuryRewardBasisPoints, TreasuryRewardAddress = obps, oaddr })
}

func TestBlockRewardSplitOffByDefault(t *testing.T) {
	miner := common.Address{0x11}
	treasury := common.Address{0x77}
	s := New()

	CreditBlockReward(s, miner, 0)

	if got := s.Balance(miner); got.Cmp(BaseBlockReward) != 0 {
		t.Fatalf("miner got %s, want the FULL reward %s (split off by default)", got, BaseBlockReward)
	}
	if got := s.Balance(treasury); got.Sign() != 0 {
		t.Fatalf("treasury got %s with the split off, want 0", got)
	}
}

func TestBlockRewardSplit8020(t *testing.T) {
	miner := common.Address{0x11}
	treasury := common.Address{0x77}
	withSplit(t, 2000, treasury) // 20% to treasury, 80% to proposer

	s := New()
	CreditBlockReward(s, miner, 0)

	wantTreasury := new(big.Int).Div(new(big.Int).Mul(BaseBlockReward, big.NewInt(2000)), big.NewInt(10000))
	wantMiner := new(big.Int).Sub(BaseBlockReward, wantTreasury)

	if got := s.Balance(treasury); got.Cmp(wantTreasury) != 0 {
		t.Fatalf("treasury got %s, want 20%% = %s", got, wantTreasury)
	}
	if got := s.Balance(miner); got.Cmp(wantMiner) != 0 {
		t.Fatalf("miner got %s, want 80%% = %s", got, wantMiner)
	}
	// conservation: the two shares must sum to exactly the reward.
	sum := new(big.Int).Add(s.Balance(treasury), s.Balance(miner))
	if sum.Cmp(BaseBlockReward) != 0 {
		t.Fatalf("treasury+miner = %s, want exactly the reward %s (split must not mint or leak)", sum, BaseBlockReward)
	}
}

// TestBlockRewardSplitRoundingGoesToProposer: with a share that does not divide
// the reward evenly, the remainder must land with the proposer (because its share
// is derived as reward-cut), and the two must still sum to the whole reward.
func TestBlockRewardSplitRoundingGoesToProposer(t *testing.T) {
	miner := common.Address{0x11}
	treasury := common.Address{0x77}
	withSplit(t, 3333, treasury) // 33.33% — deliberately not a clean divisor

	s := New()
	CreditBlockReward(s, miner, 0)

	sum := new(big.Int).Add(s.Balance(treasury), s.Balance(miner))
	if sum.Cmp(BaseBlockReward) != 0 {
		t.Fatalf("shares sum to %s, want exactly %s — the remainder must go to the proposer, not vanish", sum, BaseBlockReward)
	}
	// treasury gets the floor share; the proposer absorbs the rounding remainder.
	floorTreasury := new(big.Int).Div(new(big.Int).Mul(BaseBlockReward, big.NewInt(3333)), big.NewInt(10000))
	if got := s.Balance(treasury); got.Cmp(floorTreasury) != 0 {
		t.Fatalf("treasury got %s, want the floor %s", got, floorTreasury)
	}
}

// TestBlockRewardSplitRequiresBoth: a bps with no address, or an address with no
// bps, leaves the split off (proposer paid in full), so a half-config never burns
// the reward to the zero address.
func TestBlockRewardSplitRequiresBoth(t *testing.T) {
	miner := common.Address{0x11}

	// bps set, address zero -> off
	withSplit(t, 2000, common.Address{})
	s := New()
	CreditBlockReward(s, miner, 0)
	if got := s.Balance(miner); got.Cmp(BaseBlockReward) != 0 {
		t.Fatalf("bps-set-address-zero: miner got %s, want full %s (split must stay off)", got, BaseBlockReward)
	}
	if got := s.Balance(common.Address{}); got.Sign() != 0 {
		t.Fatalf("the zero address received %s — a half-config must never burn the reward", got)
	}
}
