package p2p

import (
	"testing"
	"time"
)

// TestTxRateLimiterThrottlesAFlood: a peer gets its burst of tokens, then is throttled once
// they run out, and recovers as time passes (tokens refill). An honest peer at normal rate
// never hits it.
func TestTxRateLimiterThrottlesAFlood(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := newTxRateLimiter(10 /*msg/s*/, 5 /*burst*/, func() time.Time { return now })
	p := PeerID("flooder")

	// Burst of 5 is allowed instantly...
	for i := 0; i < 5; i++ {
		if !rl.allow(p) {
			t.Fatalf("burst message %d denied — should be within the burst of 5", i)
		}
	}
	// ...the 6th (no time elapsed) is throttled.
	if rl.allow(p) {
		t.Fatal("a 6th message with no refill should be throttled")
	}
	// After 1 second, ~10 tokens refill (capped at burst 5) → allowed again.
	now = now.Add(time.Second)
	if !rl.allow(p) {
		t.Fatal("after a second of refill the peer should be allowed again")
	}

	// A different peer has its own independent bucket (an honest peer isn't punished for a
	// flooder sharing the node).
	if !rl.allow(PeerID("honest")) {
		t.Fatal("a fresh peer must start with a full bucket")
	}
}

// TestTxRateLimiterForget drops a peer's bucket (disconnect cleanup) without affecting others.
func TestTxRateLimiterForget(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := newTxRateLimiter(10, 2, func() time.Time { return now })
	p := PeerID("gone")
	rl.allow(p)
	rl.allow(p) // drain
	if rl.allow(p) {
		t.Fatal("expected throttle after draining the burst")
	}
	rl.forget(p) // disconnect → fresh bucket next time
	if !rl.allow(p) {
		t.Fatal("after forget, the peer should start fresh (full bucket)")
	}
}

// TestTxRateLimiterBoundsPeerMap: the bucket map never exceeds maxPeers — the least-recently
// -seen peer is evicted, so unbounded growth under identity churn is impossible.
func TestTxRateLimiterBoundsPeerMap(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := newTxRateLimiter(10, 5, func() time.Time { return now })
	rl.maxPeers = 4
	for i := 0; i < 100; i++ {
		now = now.Add(time.Second) // each new peer is "seen" later than the last
		rl.allow(PeerID(string(rune('A'+i%1000)) + string(rune(i))))
	}
	if len(rl.buckets) > 4 {
		t.Fatalf("bucket map grew to %d, want <= maxPeers 4", len(rl.buckets))
	}
}
