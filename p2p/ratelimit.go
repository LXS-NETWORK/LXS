package p2p

import (
	"sync"
	"time"
)

// txRateLimiter is a per-peer token bucket over inbound tx messages. Tx gossip
// does not penalise a peer for relaying valid-signature-but-unfundable txs (it
// cannot tell an attacker from an honest relayer), leaving a flood vector:
// well-formed txs from throwaway zero-balance keys, each forcing a decode +
// EC-recover. This limiter drops one peer's excess before that work, without
// banning it. Honest peers at a normal rate never hit it.
type txRateLimiter struct {
	mu       sync.Mutex
	buckets  map[PeerID]*tokenBucket
	rate     float64 // sustained messages/second per peer
	burst    float64 // bucket capacity (short spikes tolerated up to this)
	maxPeers int     // cap on tracked peers; over it, the least-recently-seen is evicted
	now      func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newTxRateLimiter(rate, burst float64, now func() time.Time) *txRateLimiter {
	if now == nil {
		now = time.Now
	}
	if rate <= 0 {
		rate = 200 // a healthy mesh forwards many dup txs; only a flood exceeds it
	}
	if burst <= 0 {
		burst = 2 * rate // default only an unset burst; an explicit small burst is honoured
	}
	return &txRateLimiter{buckets: make(map[PeerID]*tokenBucket), rate: rate, burst: burst, maxPeers: 8192, now: now}
}

// allow consumes one token for peer p and reports whether it was under its rate.
// A fresh peer starts with a full bucket, so normal traffic is never delayed.
func (r *txRateLimiter) allow(p PeerID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b := r.buckets[p]
	if b == nil {
		// Bound the map: at capacity, evict the least-recently-seen before
		// inserting. Peer IDs are authenticated and concurrent peers are capped
		// by the connection manager, so this only bites under sustained identity
		// churn, but it makes unbounded growth structurally impossible.
		if r.maxPeers > 0 && len(r.buckets) >= r.maxPeers {
			var oldest PeerID
			var oldestAt time.Time
			first := true
			for id, bk := range r.buckets {
				if first || bk.last.Before(oldestAt) {
					oldest, oldestAt, first = id, bk.last, false
				}
			}
			delete(r.buckets, oldest)
		}
		b = &tokenBucket{tokens: r.burst, last: now}
		r.buckets[p] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * r.rate
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// forget drops a peer's bucket, a hook for a future disconnect callback. Not
// currently wired to one; the map is bounded by maxPeers LRU eviction in
// allow(). Safe to call for an unknown peer.
func (r *txRateLimiter) forget(p PeerID) {
	r.mu.Lock()
	delete(r.buckets, p)
	r.mu.Unlock()
}
