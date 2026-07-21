package p2p

import (
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"lxs/crypto"
)

func TestScorerBansAtThreshold(t *testing.T) {
	// Frozen clock: this test pins the exact-threshold LOGIC, so no decay must run
	// between the three strikes (decay's effect over elapsed time is tested
	// separately, in TestScorerDecayForgivesOverTime).
	cur := time.Unix(1_000_000, 0)
	s := newScorer(3, DefaultScoreDecayPerSecond, scorerMaxTracked, func() time.Time { return cur })

	if s.Banned("p") {
		t.Fatal("a peer with no penalties must not be banned")
	}
	if s.Penalize("p", 1) || s.Penalize("p", 1) {
		t.Fatal("banned before reaching the threshold")
	}
	if !s.Penalize("p", 1) { // reaches 3
		t.Fatal("Penalize must report the ban when the threshold is crossed")
	}
	if !s.Banned("p") {
		t.Fatal("peer should be banned at the threshold")
	}
	// Scores are per-peer: one bad peer does not taint the rest.
	if s.Banned("q") {
		t.Fatal("an unrelated peer must stay clean")
	}
}

// A ban is a cooldown, not an exile: once a peer stops re-offending, its penalty
// decays below the threshold and it is forgiven. This is what stops a
// transiently-buggy bootstrap peer being cut permanently.
func TestScorerDecayForgivesOverTime(t *testing.T) {
	cur := time.Unix(1_000_000, 0)
	now := func() time.Time { return cur }
	s := newScorer(3, 1.0, 8192, now) // banAt 3, decay 1 point/sec

	if !s.Penalize("p", 3) {
		t.Fatal("should be banned at the threshold")
	}
	if !s.Banned("p") {
		t.Fatal("banned immediately after crossing the threshold")
	}
	cur = cur.Add(2 * time.Second) // 2 points shed -> 1 point left, below 3
	if s.Banned("p") {
		t.Fatalf("after 2s of decay the peer should be forgiven (penalty %d)", s.Penalty("p"))
	}
	// Full decay reclaims the entry entirely — no leaked map slot for an idle peer.
	cur = cur.Add(10 * time.Second)
	if s.Penalty("p") != 0 {
		t.Fatalf("penalty should have decayed to 0, got %d", s.Penalty("p"))
	}
	if _, ok := s.scores["p"]; ok {
		t.Fatal("a fully-decayed entry must be reclaimed, not kept forever")
	}
}

// A peer offending faster than the decay stays banned: decay forgives idleness,
// not a sustained flood.
func TestScorerActiveAttackerStaysBanned(t *testing.T) {
	cur := time.Unix(1_000_000, 0)
	now := func() time.Time { return cur }
	s := newScorer(3, 1.0, 8192, now)

	for i := 0; i < 5; i++ {
		s.Penalize("a", 3) // slam it well past threshold
		cur = cur.Add(1 * time.Second)
	}
	if !s.Banned("a") {
		t.Fatalf("an attacker offending faster than decay must stay banned (penalty %d)", s.Penalty("a"))
	}
}

// The peer map is bounded: identity rotation cannot grow it without limit (an
// unbounded map is itself the memory-exhaustion vector).
func TestScorerBoundedMapEvicts(t *testing.T) {
	cur := time.Unix(1_000_000, 0)
	now := func() time.Time { return cur }
	s := newScorer(100, 0, 4, now) // cap 4, no decay so eviction (not decay) must bound it

	for i := 0; i < 20; i++ {
		s.Penalize(PeerID(fmt.Sprintf("peer-%d", i)), 1) // below ban, but a live entry
		cur = cur.Add(1 * time.Millisecond)              // distinct last-seen for LRU order
	}
	if len(s.scores) > 4 {
		t.Fatalf("scorer map not bounded: %d entries, cap is 4", len(s.scores))
	}
}

// A peer that keeps sending garbage blocks is banned, and once banned its
// traffic is dropped before it is processed.
func TestGossipBansAndThenDropsABadPeer(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 42})
	// Threshold 2, not 3: penalties decay over real time, so three messages sent
	// in nonzero wall-clock land a hair under 3. Three malformed messages are
	// comfortably over a threshold of two. Exact-threshold crossing is pinned
	// with a frozen clock in TestScorerBansAtThreshold.
	scorer := NewScorer(2)

	attacker := newTestNode(t, sw, "attacker", gen)
	victim := newTestNode(t, sw, "victim", gen, WithScorer(scorer))

	garbage := func() {
		junk := make([]byte, 8)
		rand.Read(junk) // not valid JSON → a rejection
		attacker.n.Publish(TopicBlocks, junk)
	}

	// Three malformed messages carry the attacker past the ban threshold.
	for i := 0; i < 3; i++ {
		garbage()
	}
	if !scorer.Banned("attacker") {
		t.Fatalf("attacker should be banned after 3 bad blocks (penalty %d)", scorer.Penalty("attacker"))
	}

	before := victim.g.Snapshot()
	garbage() // a fourth message, now from a banned peer
	after := victim.g.Snapshot()

	if after.Received != before.Received {
		t.Fatal("a banned peer's message was still received and processed")
	}
	if after.Dropped != before.Dropped+1 {
		t.Fatalf("a banned peer's message was not dropped: dropped %d -> %d", before.Dropped, after.Dropped)
	}
}
