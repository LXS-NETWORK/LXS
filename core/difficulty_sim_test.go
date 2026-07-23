package core

import (
	"testing"

	"lxs/types"
)

// hashrateAt returns the network hashrate for a given block height, letting a
// simulation model a miner that joins or leaves partway through.
type hashrateAt func(height int) float64

// simulate mirrors requiredDifficultyLocked (partial window, minWindow 2) exactly,
// driving it with a (possibly time-varying) hashrate. Deterministic — solvetime =
// difficulty/hashrate — so it isolates the controller from PoW luck. Returns the
// per-block solvetimes (seconds).
func simulate(genesisDiff uint64, hr hashrateAt, nBlocks int) []float64 {
	const N = LwmaWindow
	const minWindow = 2
	headers := []*types.Header{{Timestamp: 0, Difficulty: genesisDiff}}
	out := make([]float64, 0, nBlocks)
	for i := 1; i <= nBlocks; i++ {
		parentH := i - 1
		var reqDiff uint64
		w := parentH
		if w > N {
			w = N
		}
		if w < minWindow {
			reqDiff = genesisDiff
		} else {
			reqDiff = lwmaDifficulty(headers[len(headers)-(w+1):])
		}
		st := float64(reqDiff) / hr(i) // seconds
		out = append(out, st)
		headers = append(headers, &types.Header{
			Timestamp:  headers[len(headers)-1].Timestamp + int64(st*1000),
			Difficulty: reqDiff,
		})
	}
	return out
}

const targetS = 240.0

// countOutside returns how many of sts[from:] fall outside ±tol of the target.
func countOutside(sts []float64, from int, tol float64) int {
	c := 0
	for i := from; i < len(sts); i++ {
		if sts[i] < targetS*(1-tol) || sts[i] > targetS*(1+tol) {
			c++
		}
	}
	return c
}

// Genesis difficulty calibrated to the laptop's ~1.2 MH/s must give ~4-minute
// blocks from the very first block, and stay there.
func TestDifficultyLaptopFromBlockOne(t *testing.T) {
	const genesis = 288_000_000 // 1.2 MH/s × 240 s
	sts := simulate(genesis, func(int) float64 { return 1_200_000 }, 60)
	t.Logf("laptop: block1=%.0fs block2=%.0fs block5=%.0fs block30=%.0fs block60=%.0fs", sts[0], sts[1], sts[4], sts[29], sts[59])
	if sts[0] < targetS*0.8 || sts[0] > targetS*1.2 {
		t.Fatalf("block 1 = %.0fs, want within ±20%% of %.0fs (calibrated genesis must not solve in seconds)", sts[0], targetS)
	}
	if n := countOutside(sts, 3, 0.25); n > 3 {
		t.Fatalf("%d blocks after block 3 strayed >25%% from target — LWMA not holding", n)
	}
}

// A laptop joining a running server-only chain (a 6× hashrate jump) must be
// re-absorbed quickly — the whole point of the shorter window.
func TestDifficultyAbsorbsLaptopJoining(t *testing.T) {
	// Server ~200 kH/s alone, calibrated genesis; laptop (+1.2 MH/s) joins at block 30.
	const genesis = 48_000_000 // 200 kH/s × 240 s
	hr := func(h int) float64 {
		if h < 30 {
			return 200_000
		}
		return 1_400_000 // server + laptop
	}
	sts := simulate(genesis, hr, 30+LwmaWindow+20)
	// How many blocks after the jump are still fast (>25% below target)?
	fast := 0
	for i := 30; i < len(sts) && sts[i] < targetS*0.75; i++ {
		fast++
	}
	t.Logf("laptop joins at block 30: fast blocks before re-lock = %d; it MUST re-lock within one window (%d)", fast, LwmaWindow)
	// LWMA's guarantee: a hashrate jump is fully absorbed within one averaging
	// window. It cannot be instant (that would make the controller trivially
	// gameable) — the honest bound is "within a window", which the shorter 45 gives.
	if fast > LwmaWindow {
		t.Fatalf("laptop join caused %d fast blocks — more than one window (%d); LWMA is not absorbing the jump", fast, LwmaWindow)
	}
	// Once past a full window after the jump, it must sit on target.
	if n := countOutside(sts, 30+LwmaWindow, 0.3); n > 4 {
		t.Fatalf("%d blocks a full window after the jump still strayed >30%% — not re-stabilised", n)
	}
}
