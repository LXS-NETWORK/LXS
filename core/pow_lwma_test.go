package core

import (
	"math/big"
	"testing"

	"lxs/types"
)

// lwmaWindowConst builds an LwmaWindow+1 header window with a constant solvetime and a
// constant per-block difficulty — the steady-state input for reasoning about equilibrium.
func lwmaWindowConst(startTs, solvetime int64, diff uint64) []*types.Header {
	w := make([]*types.Header, LwmaWindow+1)
	ts := startTs
	for i := range w {
		w[i] = &types.Header{Timestamp: ts, Difficulty: diff}
		ts += solvetime
	}
	return w
}

// lwmaWindowN builds an N+1 header window (N solvetimes) with constant solvetime and
// difficulty — for testing PARTIAL windows on a young chain, where N < LwmaWindow.
func lwmaWindowN(n int, startTs, solvetime int64, diff uint64) []*types.Header {
	w := make([]*types.Header, n+1)
	ts := startTs
	for i := range w {
		w[i] = &types.Header{Timestamp: ts, Difficulty: diff}
		ts += solvetime
	}
	return w
}

// A young chain (a partial window, far fewer than LwmaWindow blocks) must still
// retarget: LWMA derives N from the window length. With blocks arriving 8× slower
// than target, difficulty must drop sharply — this is exactly what lets a fresh
// chain fall from an over-set genesis difficulty to the real hashrate in a few
// blocks. If N were hardcoded to LwmaWindow, this short window would panic on an
// out-of-range index, so the test also guards the derivation itself.
func TestLwmaPartialWindowRetargetsDown(t *testing.T) {
	const D = 1_000_000
	got := lwmaDifficulty(lwmaWindowN(4, 1_000_000_000, 8*TargetBlockTime, D))
	if got >= D/3 {
		t.Fatalf("with 8×-slow blocks over a 4-block window, difficulty = %d; expected a sharp drop (well below D/3 = %d)", got, D/3)
	}
	// And a partial window exactly on target keeps the same avgD·0.99 equilibrium
	// as a full window — proving the math scales with N.
	eq := lwmaDifficulty(lwmaWindowN(5, 1_000_000_000, TargetBlockTime, D))
	if want := uint64(D * 99 / 100); eq != want {
		t.Fatalf("on-target 5-block window = %d, want %d (avgD·0.99)", eq, want)
	}
}

// At equilibrium (every block exactly on target) LWMA returns avgD·0.99 exactly — the
// intended slight downward bias that keeps mean spacing at ~T.
func TestLwmaEquilibriumIsStable(t *testing.T) {
	const D = 1_000_000
	got := lwmaDifficulty(lwmaWindowConst(1_000_000_000, TargetBlockTime, D))
	if want := uint64(D * 99 / 100); got != want {
		t.Fatalf("on-target difficulty = %d, want %d (avgD·0.99)", got, want)
	}
}

// At a very high per-block difficulty the 90-block difficulty sum must not overflow.
// D = 2^60 makes 90·D exceed uint64, which under the old uint64 accumulator wrapped
// the average and mis-set the retarget (deterministic, so never a fork, but wrong at
// ~600 TH/s-class hashrate on a chain that is never patched). At equilibrium the
// answer is still exactly avgD·0.99, so a wrapped sum yields a wildly different value.
func TestLwmaDifficultySumDoesNotOverflow(t *testing.T) {
	const D = uint64(1) << 60 // ~1.15e18; 90·D ≈ 1.04e20 > uint64 max ≈ 1.84e19
	got := lwmaDifficulty(lwmaWindowConst(1_000_000_000, TargetBlockTime, D))
	// want = D·99/100, computed in big.Int so the TEST itself does not overflow.
	want := new(big.Int).Div(new(big.Int).Mul(new(big.Int).SetUint64(D), big.NewInt(99)), big.NewInt(100)).Uint64()
	if got != want {
		t.Fatalf("at D=2^60 difficulty = %d, want %d (avgD·0.99) — the 90-block sum overflowed uint64", got, want)
	}
}

// LWMA is multiplicative: twice the hashrate (blocks arrive twice as fast) roughly doubles
// difficulty; half the hashrate roughly halves it. This is the property that makes it
// collapse-proof — difficulty tracks hashrate proportionally.
func TestLwmaTracksHashrate(t *testing.T) {
	const D = 1_000_000
	onTarget := lwmaDifficulty(lwmaWindowConst(1_000_000_000, TargetBlockTime, D))
	fast := lwmaDifficulty(lwmaWindowConst(1_000_000_000, TargetBlockTime/2, D)) // 2x hashrate
	slow := lwmaDifficulty(lwmaWindowConst(1_000_000_000, TargetBlockTime*2, D)) // 0.5x hashrate

	if fast <= onTarget {
		t.Fatalf("faster blocks must raise difficulty: fast=%d onTarget=%d", fast, onTarget)
	}
	if slow >= onTarget {
		t.Fatalf("slower blocks must lower difficulty: slow=%d onTarget=%d", slow, onTarget)
	}
	// magnitude: ~2x fast -> ~1.98x, ~2x slow -> ~0.495x (multiplicative, not additive).
	if fast < D*3/2 || fast > D*5/2 {
		t.Fatalf("2x-fast difficulty = %d, want ~2x of %d", fast, D)
	}
	if slow > D*3/4 || slow < D/4 {
		t.Fatalf("2x-slow difficulty = %d, want ~0.5x of %d", slow, D)
	}
}

// A single far-future timestamp is clamped to 6·T, so a miner cannot post-date one block
// to crater difficulty. Difficulty drops at most as if that block took 6·T, not infinity.
func TestLwmaClampsFarFutureSolvetime(t *testing.T) {
	const D = 1_000_000
	w := lwmaWindowConst(1_000_000_000, TargetBlockTime, D)
	// push the last block's timestamp astronomically far into the future.
	w[LwmaWindow].Timestamp = w[LwmaWindow-1].Timestamp + TargetBlockTime*1_000_000
	got := lwmaDifficulty(w)
	// with the clamp, the worst that one 6T block does is a bounded dip; without it the
	// huge solvetime would drive difficulty to the MinDifficulty floor.
	if got <= MinDifficulty {
		t.Fatalf("clamp failed: a far-future timestamp cratered difficulty to %d", got)
	}
	if got >= D {
		t.Fatalf("a very slow (clamped) block should still lower difficulty somewhat, got %d", got)
	}
}

// Pure + deterministic: identical windows yield identical difficulty (or nodes fork).
func TestLwmaDeterministic(t *testing.T) {
	w := lwmaWindowConst(42, TargetBlockTime*3/2, 777_777)
	if lwmaDifficulty(w) != lwmaDifficulty(w) {
		t.Fatal("lwmaDifficulty is not deterministic")
	}
}
