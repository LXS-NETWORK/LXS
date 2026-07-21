package state

import (
	"math/big"
	"testing"
)

// The reward halves once per interval, and holds flat within an interval.
func TestBlockRewardHalves(t *testing.T) {
	two := new(big.Int).Set(BaseBlockReward)
	one := new(big.Int).Rsh(two, 1)
	half := new(big.Int).Rsh(two, 2)

	cases := []struct {
		height uint64
		want   *big.Int
	}{
		{0, two},
		{1, two},
		{HalvingInterval - 1, two},
		{HalvingInterval, one},
		{2*HalvingInterval - 1, one},
		{2 * HalvingInterval, half},
	}
	for _, c := range cases {
		if got := BlockRewardAt(c.height); got.Cmp(c.want) != 0 {
			t.Fatalf("BlockRewardAt(%d) = %s, want %s", c.height, got, c.want)
		}
	}

	// Enough halvings and the reward rounds to zero; only fees remain.
	if BlockRewardAt(200*HalvingInterval).Sign() != 0 {
		t.Fatal("after enough halvings the reward must reach zero")
	}
}

// Total issuance is finite: summed across every era until the reward rounds to
// zero, it converges, so supply is bounded.
func TestTotalIssuanceIsFinite(t *testing.T) {
	total := new(big.Int)
	interval := new(big.Int).SetUint64(HalvingInterval)
	for era := uint64(0); era < 1000; era++ {
		reward := BlockRewardAt(era * HalvingInterval)
		if reward.Sign() == 0 {
			break // issuance has ended; the sum is complete and finite
		}
		total.Add(total, new(big.Int).Mul(reward, interval))
	}

	// Geometric closed form: sum = 2 * base * interval. Integer halving loses a
	// little in the tail, so the real total is at or just under this bound.
	bound := new(big.Int).Mul(BaseBlockReward, new(big.Int).SetUint64(2*HalvingInterval))
	if total.Cmp(bound) > 0 {
		t.Fatalf("total issuance %s exceeds the geometric bound %s", total, bound)
	}
	oneEra := new(big.Int).Mul(BaseBlockReward, interval)
	if total.Cmp(oneEra) <= 0 {
		t.Fatalf("total issuance %s is implausibly small", total)
	}
}
