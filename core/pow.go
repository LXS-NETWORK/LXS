package core

import (
	"fmt"
	"math/big"
	"sync/atomic"

	"lxs/types"
)

// hashCounter counts PoW hashes tried while mining, process-wide. A mining dashboard
// samples it over time to derive a hashrate. Monotonic; resets on restart.
var hashCounter atomic.Uint64

// HashCount is the cumulative number of PoW hashes this process has tried while mining.
func HashCount() uint64 { return hashCounter.Load() }

// Proof of work. A block is valid only if its header hashes below a target set by its
// Difficulty, and Difficulty is derived (LWMA over the recent window), not chosen. The only
// free variable is the Nonce, movable below target only by ~Difficulty tries.

// MinDifficulty is the floor. Difficulty 1 makes the target the whole hash space,
// so any nonce wins: in-process tests get instant blocks with no grinding.
const MinDifficulty uint64 = 1

// TargetBlockTime is LWMA's target solvetime in ms. LWMA drives mean spacing to ~T:
// 240000 ms = 4 minutes; with the halving schedule this spreads the 100M mined supply
// over ~500 years.
const TargetBlockTime int64 = 240000

// maxTarget is 2^256 - 1: the target at difficulty 1, where every hash wins.
var maxTarget = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// powTarget is the ceiling a valid block hash must fall under. Higher difficulty ->
// smaller target -> more hashes needed.
func powTarget(difficulty uint64) *big.Int {
	if difficulty == 0 {
		difficulty = 1 // never divide by zero
	}
	return new(big.Int).Div(maxTarget, new(big.Int).SetUint64(difficulty))
}

// satisfiesPoW reports whether the header's hash falls under its claimed
// difficulty's target. It does not check the difficulty is correct (the caller's
// job); both must pass, since a hash under a self-lowered target is worthless.
func satisfiesPoW(h *types.Header) bool {
	hash := h.Hash()
	return new(big.Int).SetBytes(hash[:]).Cmp(powTarget(h.Difficulty)) <= 0
}

// LwmaWindow is the number of recent blocks LWMA averages. 45 blocks (~3 hours at
// the 4-minute target) reacts fast to hashrate that comes and goes — a home miner
// opening and closing the app — while staying inside zawy's stable band, so the
// chain re-finds the target within a shorter run of blocks after a hashrate jump.
const LwmaWindow = 45

// lwmaDifficulty derives a block's difficulty from a window of the LwmaWindow+1 most recent
// headers (oldest first; window[LwmaWindow] is the parent), using zawy's LWMA-1. It sets
// difficulty from a LINEARLY-WEIGHTED average of recent solvetimes (recent blocks weigh
// more), so difficulty tracks hashrate within hours and — being multiplicative /
// scale-invariant — can never collapse to a trivially-mineable floor while hashrate is
// merely low (the failure mode of the old additive retarget). Because it targets the block
// time directly, mean spacing settles at ~T, restoring the intended emission schedule.
// Reference: zawy12/difficulty-algorithms (LWMA-1). Pure + deterministic: every node
// computes the same value from the same window, or the network forks.
func lwmaDifficulty(window []*types.Header) uint64 {
	// N derives from the window length (window has N+1 headers: window[0] is the
	// block just before the averaging span). Deriving it — rather than hardcoding
	// LwmaWindow — lets the retarget run over a PARTIAL window on a young chain, so
	// difficulty tracks real hashrate from the first few blocks instead of staying
	// frozen at the genesis value until block LwmaWindow. All guards/weights below
	// already scale with N, so the math is valid for any N >= 2.
	N := len(window) - 1
	T := TargetBlockTime // target solvetime, same unit as Header.Timestamp (ms)

	var L int64 // Σ i·solvetime_i  (linear weights 1..N; recent blocks weigh more)
	// sumD accumulates the window's difficulties in a big.Int, NOT a uint64: 90
	// difficulties each up to ~2^57 overflow uint64 (90·2^57 > 2^64), which at
	// ~600 TH/s-class hashrate would wrap the average and mis-set the retarget.
	// It never forked (every node wraps identically), but on an immutable chain the
	// math must stay correct at any hashrate the uint64 difficulty field can hold.
	// The reference LWMA (Monero/zawy) sidesteps this by differencing cumulative
	// difficulties; we have per-header difficulty only, so we widen the sum instead.
	sumD := new(big.Int)
	// solvetime_i = timestamps[i] - timestamps[i-1]; window[0] is the block just before the
	// N-block averaging window, so the first solvetime is a real block interval, not a bias.
	prev := window[0].Timestamp
	for i := 1; i <= N; i++ {
		ts := window[i].Timestamp
		if ts <= prev {
			ts = prev + 1 // monotonic: solvetime >= 1 (timestamps are validated monotonic; defensive)
		}
		st := ts - prev
		if st > 6*T {
			st = 6 * T // clamp one solvetime, so a far-future timestamp can't crater difficulty
		}
		L += int64(i) * st
		prev = ts
		sumD.Add(sumD, new(big.Int).SetUint64(window[i].Difficulty))
	}
	if minL := int64(N) * int64(N) * T / 20; L < minL {
		L = minL // guard: bounds how far difficulty can jump up in a single window
	}
	avgD := new(big.Int).Div(sumD, big.NewInt(int64(N)))

	// next = avgD · N(N+1)·T·99 / (200·L)   (= avgD · k · T · 0.99 / L, k = N(N+1)/2).
	// All big.Int: avgD (up to the full uint64 difficulty range) times N(N+1)·T·99
	// (~2^44) far exceeds uint64.
	next := new(big.Int).Mul(avgD, big.NewInt(int64(N)*int64(N+1)*T*99))
	next.Div(next, big.NewInt(200*L))
	// Saturate at the uint64 ceiling (the Header.Difficulty field's limit) rather
	// than silently wrap — deterministic, and only reachable if difficulty is
	// already at the field's maximum.
	var d uint64
	if next.IsUint64() {
		d = next.Uint64()
	} else {
		d = ^uint64(0)
	}
	if d < MinDifficulty {
		d = MinDifficulty
	}
	return d
}

// VerifyHeaderPoW is the sync layer's cheap work barrier over a header chain, run before
// any body download. See the body for exactly what it does and does not check.
func VerifyHeaderPoW(h *types.Header) error {
	// The work barrier: h's nonce must satisfy the difficulty h CLAIMS, so a forged header
	// still costs real work before we download any body. It does NOT re-derive the difficulty
	// via LWMA — that needs the block's full LwmaWindow ancestry, which the header-chain sync
	// stage does not hold — so the AUTHORITATIVE difficulty check is in InsertBlock
	// (requiredDifficultyLocked). A peer claiming an artificially low difficulty still had to
	// meet that target, accrues little total work (loses fork choice), and is rejected at
	// InsertBlock; the residual is at most a bounded body download, not a consensus risk.
	if h.Difficulty < MinDifficulty {
		return fmt.Errorf("%w: difficulty %d below floor", ErrBadDifficulty, h.Difficulty)
	}
	if !satisfiesPoW(h) {
		return fmt.Errorf("%w: %s", ErrBadPoW, h.Hash().Hex())
	}
	return nil
}

// PowTarget is the hash ceiling for a difficulty, exported for the mining pool:
// the pool hands workers an EASIER target (a fraction of the block difficulty) so
// weak machines produce steady "shares" that prove contributed work, and the pool
// itself re-checks every share against both targets with this same function. One
// implementation on both sides, or a pool-accepted share could be block-invalid.
func PowTarget(difficulty uint64) *big.Int { return powTarget(difficulty) }

// Grind searches nonces from start until the header hashes at or under target, or
// stop closes. It returns the nonce to RESUME from: on success the winning nonce
// (caller resumes at nonce+1 to hunt more shares on the same header), on abort the
// next untried nonce. Pool workers need both behaviours; solo mining is the special
// case target=PowTarget(h.Difficulty), start=0. Mutates h (Nonce + cached hash) —
// the caller owns the header.
//
// start matters for pools: two workers grinding the same header from 0 duplicate
// every hash; each starting at a random 64-bit offset makes overlap negligible.
func Grind(h *types.Header, target *big.Int, start uint64, stop <-chan struct{}) (uint64, bool) {
	for nonce := start; ; nonce++ {
		select {
		case <-stop:
			return nonce, false
		default:
		}
		h.Nonce = nonce
		h.InvalidateHash()
		hash := h.Hash()
		hashCounter.Add(1)
		if new(big.Int).SetBytes(hash[:]).Cmp(target) <= 0 {
			return nonce, true
		}
	}
}

// mine grinds the Nonce until the header satisfies its target, or aborts via the
// stop channel (returning false). The header's Difficulty must already be set.
func mine(h *types.Header, stop <-chan struct{}) bool {
	_, ok := Grind(h, powTarget(h.Difficulty), 0, stop)
	return ok
}
