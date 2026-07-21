package state

import (
	"math/big"
	"testing"
)

// CumulativeIssued is the expected-issuance term in CheckConservation, so it must
// equal the REAL per-block ledger Σ_{i=1}^{h} BlockRewardAt(i) exactly. Any drift
// raises a false "conservation VIOLATED" CRITICAL — and on an immutable chain that
// alarm can never be silenced. The naive interval*reward-per-era sum over-counts the
// halving-boundary block (BlockRewardAt halves AT the boundary, and height 0 is
// genesis with no reward), so this pins the closed form to a brute-force sum across
// the first several halving boundaries.
func TestCumulativeIssuedMatchesPerBlockLedger(t *testing.T) {
	// Brute-force reference: sum the actual per-block reward, height 1..h.
	ledger := func(h uint64) *big.Int {
		sum := new(big.Int)
		for i := uint64(1); i <= h; i++ {
			sum.Add(sum, BlockRewardAt(i))
		}
		return sum
	}

	for _, h := range []uint64{
		0, 1, 2, 100,
		HalvingInterval - 1,     // last block of era 0
		HalvingInterval,         // FIRST halving — the boundary block pays 25, not 50
		HalvingInterval + 1,     //
		2 * HalvingInterval,     // second halving boundary
		2*HalvingInterval + 123, //
		3 * HalvingInterval,     // third
	} {
		got := CumulativeIssued(h)
		want := ledger(h)
		if got.Cmp(want) != 0 {
			t.Fatalf("CumulativeIssued(%d) = %s, but Σ BlockRewardAt(1..h) = %s (delta %s) — false conservation alarm",
				h, got, want, new(big.Int).Sub(got, want))
		}
	}
}
