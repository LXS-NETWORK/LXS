package health

import (
	"sort"
	"sync"
	"time"
)

// Feedback control that adapts the node's own behaviour from the telemetry the other
// components gather. Two adaptations, both pure and testable:
//
//   - RankSyncPeers: adaptive peer selection. Sync from the peers with the lowest Scorer
//     penalty, never from a banned one. The same scores that get a peer disconnected steer
//     traffic away from it well before it crosses the ban line.
//   - AdaptiveBackoff: adaptive sync cadence. Widen the interval while the node stays caught
//     up (spare the network), tighten it the moment it falls behind (converge fast). AIMD,
//     the shape that keeps TCP stable, bounded so it can neither hammer nor stall.

// RankSyncPeers returns the peers worth syncing from, best-first: banned peers are dropped
// entirely, the rest ordered by ascending penalty with the id as a deterministic tie-break.
// The caller picks from the front, optionally spreading load across the equally-good leaders
// (see LeadingTier).
func RankSyncPeers(peers []PeerHealth) []PeerHealth {
	out := make([]PeerHealth, 0, len(peers))
	for _, p := range peers {
		if p.Banned {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Penalty != out[j].Penalty {
			return out[i].Penalty < out[j].Penalty
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// LeadingTier returns the ids of the equally-best peers (all sharing the lowest penalty)
// from a RankSyncPeers result — the set a caller can pick among at random to prefer good
// peers without hammering a single one. Empty if there are no eligible peers.
func LeadingTier(ranked []PeerHealth) []string {
	if len(ranked) == 0 {
		return nil
	}
	best := ranked[0].Penalty
	var ids []string
	for _, p := range ranked {
		if p.Penalty != best {
			break
		}
		ids = append(ids, p.ID)
	}
	return ids
}

// PickGapSyncPeer chooses whom to catch up from when a block arrives whose parent
// we lack. `from` is the peer that delivered that orphan, so it is provably ahead
// of us — but binding the catch-up to it lets a flaky peer that is penalised yet
// not-yet-banned keep dribbling orphans to pin our sync slot to itself, starving
// the healthier peers. So `from` is used only when it is ALSO in the healthiest
// tier (ahead and healthy — the ideal source); otherwise a top-tier peer is chosen
// and re-gossip re-triggers the gap if they were not in fact ahead. Returns
// ("", false) when there is no un-banned peer to sync from at all.
func PickGapSyncPeer(from string, peers []PeerHealth) (string, bool) {
	leaders := LeadingTier(RankSyncPeers(peers))
	if len(leaders) == 0 {
		return "", false
	}
	for _, id := range leaders {
		if id == from {
			return from, true
		}
	}
	return leaders[0], true
}

// AdaptiveBackoff is a bounded AIMD interval. Relax (healthy) multiplies the interval up
// toward Max; Tighten (stressed) multiplies it down toward Min. Both clamp, so the cadence
// stays in [Min, Max] no matter how long a run of either signal lasts. Concurrency-safe.
type AdaptiveBackoff struct {
	Min, Max     time.Duration
	Grow, Shrink float64 // Grow >= 1 widens on Relax; 0 < Shrink <= 1 tightens on Tighten

	mu   sync.Mutex
	cur  time.Duration
	init bool
}

func (a *AdaptiveBackoff) ensure() {
	if a.init {
		return
	}
	if a.Min <= 0 {
		a.Min = time.Second
	}
	if a.Max < a.Min {
		a.Max = 10 * a.Min
	}
	if a.Grow < 1 {
		a.Grow = 1.5
	}
	if a.Shrink <= 0 || a.Shrink > 1 {
		a.Shrink = 0.5
	}
	if a.cur < a.Min || a.cur > a.Max {
		a.cur = a.Min
	}
	a.init = true
}

func (a *AdaptiveBackoff) clamp(d time.Duration) time.Duration {
	if d < a.Min {
		return a.Min
	}
	if d > a.Max {
		return a.Max
	}
	return d
}

// Relax widens the interval (the node is healthy — sync less often) and returns the new value.
func (a *AdaptiveBackoff) Relax() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ensure()
	a.cur = a.clamp(time.Duration(float64(a.cur) * a.Grow))
	return a.cur
}

// Tighten shrinks the interval (the node is behind — sync harder) and returns the new value.
func (a *AdaptiveBackoff) Tighten() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ensure()
	a.cur = a.clamp(time.Duration(float64(a.cur) * a.Shrink))
	return a.cur
}

// Value is the current interval.
func (a *AdaptiveBackoff) Value() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ensure()
	return a.cur
}
