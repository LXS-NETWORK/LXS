package health

import (
	"testing"
	"time"
)

// TestRankSyncPeersPrefersGoodAndDropsBanned: banned peers are excluded, the rest come
// back ordered by ascending penalty (best first), ties broken by id.
func TestRankSyncPeersPrefersGoodAndDropsBanned(t *testing.T) {
	peers := []PeerHealth{
		{ID: "c", Penalty: 5},
		{ID: "banned", Penalty: 99, Banned: true},
		{ID: "a", Penalty: 0},
		{ID: "b", Penalty: 0},
		{ID: "d", Penalty: 20},
	}
	ranked := RankSyncPeers(peers)
	got := ""
	for _, p := range ranked {
		got += p.ID
	}
	if got != "abcd" {
		t.Fatalf("ranked order = %q, want \"abcd\" (banned excluded, penalty asc, id tie-break)", got)
	}
}

func TestLeadingTierGroupsTheBest(t *testing.T) {
	ranked := RankSyncPeers([]PeerHealth{
		{ID: "a", Penalty: 0}, {ID: "b", Penalty: 0}, {ID: "c", Penalty: 3},
	})
	tier := LeadingTier(ranked)
	if len(tier) != 2 || tier[0] != "a" || tier[1] != "b" {
		t.Fatalf("leading tier = %v, want [a b]", tier)
	}
	if LeadingTier(nil) != nil {
		t.Fatal("leading tier of nothing should be nil")
	}
}

func TestRankSyncPeersAllBannedIsEmpty(t *testing.T) {
	ranked := RankSyncPeers([]PeerHealth{{ID: "x", Banned: true}, {ID: "y", Banned: true}})
	if len(ranked) != 0 {
		t.Fatalf("all-banned ranked = %v, want empty (never sync from a liar)", ranked)
	}
}

// TestAdaptiveBackoffConverges: repeated Relax climbs to Max and stops; repeated Tighten
// falls to Min and stops. The value never escapes [Min, Max].
func TestAdaptiveBackoffConverges(t *testing.T) {
	a := &AdaptiveBackoff{Min: time.Second, Max: 30 * time.Second, Grow: 2, Shrink: 0.5}
	// climb
	for i := 0; i < 20; i++ {
		v := a.Relax()
		if v < time.Second || v > 30*time.Second {
			t.Fatalf("relaxed value %s escaped [1s,30s]", v)
		}
	}
	if a.Value() != 30*time.Second {
		t.Fatalf("after many Relax, value = %s, want 30s (Max)", a.Value())
	}
	// dive
	for i := 0; i < 20; i++ {
		v := a.Tighten()
		if v < time.Second || v > 30*time.Second {
			t.Fatalf("tightened value %s escaped [1s,30s]", v)
		}
	}
	if a.Value() != time.Second {
		t.Fatalf("after many Tighten, value = %s, want 1s (Min)", a.Value())
	}
}

// TestAdaptiveBackoffReactsToStress: one Tighten after being relaxed measurably shortens
// the interval — the node syncs harder the moment it falls behind.
func TestAdaptiveBackoffReactsToStress(t *testing.T) {
	a := &AdaptiveBackoff{Min: time.Second, Max: 60 * time.Second, Grow: 2, Shrink: 0.5}
	a.Relax()
	a.Relax()
	a.Relax() // 8s
	before := a.Value()
	after := a.Tighten()
	if after >= before {
		t.Fatalf("Tighten did not shorten the interval: before=%s after=%s", before, after)
	}
}

// TestAdaptiveBackoffDefaults: a zero-value-ish backoff still behaves (defaults kick in).
func TestAdaptiveBackoffDefaults(t *testing.T) {
	a := &AdaptiveBackoff{} // all defaults
	if v := a.Value(); v <= 0 {
		t.Fatalf("defaulted value = %s, want positive", v)
	}
	if a.Relax() <= 0 || a.Tighten() <= 0 {
		t.Fatal("defaulted adjustments must stay positive")
	}
}

// TestPickGapSyncPeerAvoidsFlakyAnnouncer: when a gap is detected, a penalised
// (but not banned) announcer must NOT pin our catch-up to itself — a healthier peer
// is chosen instead. When the announcer IS in the healthiest tier it is used (ahead
// AND healthy). With no un-banned peers, nothing is chosen.
func TestPickGapSyncPeerAvoidsFlakyAnnouncer(t *testing.T) {
	peers := []PeerHealth{
		{ID: "flaky", Penalty: 8},
		{ID: "good1", Penalty: 1},
		{ID: "good2", Penalty: 1},
		{ID: "banned", Penalty: 0, Banned: true},
	}

	// Announcer is the flaky one: must be steered away to a top-tier peer.
	got, ok := PickGapSyncPeer("flaky", peers)
	if !ok || got == "flaky" {
		t.Fatalf("flaky announcer chosen (%q, ok=%v) — a penalised peer pinned the sync slot", got, ok)
	}
	if got != "good1" && got != "good2" {
		t.Fatalf("gap target = %q, want a leading-tier peer", got)
	}

	// A banned peer must never be chosen even if it is the announcer.
	if got, _ := PickGapSyncPeer("banned", peers); got == "banned" {
		t.Fatal("banned peer chosen as sync target")
	}

	// Announcer already in the healthiest tier: use it (ahead and healthy).
	if got, ok := PickGapSyncPeer("good2", peers); !ok || got != "good2" {
		t.Fatalf("healthy announcer = %q ok=%v, want it used directly (good2)", got, ok)
	}

	// No eligible peers -> no target.
	if _, ok := PickGapSyncPeer("x", []PeerHealth{{ID: "z", Banned: true}}); ok {
		t.Fatal("a target was chosen when every peer is banned")
	}
}
