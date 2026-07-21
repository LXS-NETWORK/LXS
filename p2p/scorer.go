package p2p

import (
	"math"
	"sync"
	"time"
)

// Scorer tracks per-peer misbehaviour and bans peers whose penalty crosses a
// threshold; a banned peer's messages are dropped before decoding.
//
// Penalties decay over time (DefaultScoreDecayPerSecond), so a ban acts as a
// cooldown rather than a permanent block: a peer that stops offending is
// eventually forgiven, while one offending faster than the decay stays banned.
// The peer map is bounded (scorerMaxTracked) with LRU eviction — an unbounded
// identity-keyed map is a memory-exhaustion vector under key rotation.
type Scorer struct {
	mu         sync.Mutex
	scores     map[PeerID]*scoreEntry
	banAt      float64
	decay      float64 // points shed per second
	maxTracked int
	now        func() time.Time
}

type scoreEntry struct {
	points float64
	last   time.Time
}

const (
	// DefaultBanThreshold is the penalty at which a peer is cut off (1 point per
	// rejected message by convention).
	DefaultBanThreshold = 50

	// DefaultScoreDecayPerSecond is the penalty decay rate. At 0.1/s a peer banned
	// at 50 points is forgiven after ~500s without re-offending.
	DefaultScoreDecayPerSecond = 0.1

	// scorerMaxTracked bounds the peer map (matches the tx/RPC limiters).
	scorerMaxTracked = 8192
)

func NewScorer(banAt int) *Scorer {
	return newScorer(banAt, DefaultScoreDecayPerSecond, scorerMaxTracked, nil)
}

// newScorer allows tests to inject a clock and small caps.
func newScorer(banAt int, decay float64, maxTracked int, now func() time.Time) *Scorer {
	if banAt <= 0 {
		banAt = DefaultBanThreshold
	}
	if maxTracked < 1 {
		maxTracked = scorerMaxTracked
	}
	if now == nil {
		now = time.Now
	}
	return &Scorer{
		scores:     make(map[PeerID]*scoreEntry),
		banAt:      float64(banAt),
		decay:      decay,
		maxTracked: maxTracked,
		now:        now,
	}
}

// current returns a peer's decayed points and updates its timestamp. A peer with
// no entry has 0 points; an entry that decays to 0 is deleted. Caller holds the lock.
func (s *Scorer) current(p PeerID, now time.Time) float64 {
	e, ok := s.scores[p]
	if !ok {
		return 0
	}
	if elapsed := now.Sub(e.last).Seconds(); elapsed > 0 {
		e.points = math.Max(0, e.points-elapsed*s.decay)
		e.last = now
	}
	if e.points == 0 {
		delete(s.scores, p)
		return 0
	}
	return e.points
}

// Penalize adds points to a peer's tally and reports whether it is now banned.
func (s *Scorer) Penalize(p PeerID, points int) (banned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	cur := s.current(p, now)
	e, ok := s.scores[p]
	if !ok {
		if len(s.scores) >= s.maxTracked {
			s.evict(now)
		}
		e = &scoreEntry{}
		s.scores[p] = e
	}
	e.points = cur + float64(points)
	e.last = now
	return e.points >= s.banAt
}

// Banned reports whether a peer is over the threshold after decay.
func (s *Scorer) Banned(p PeerID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current(p, s.now()) >= s.banAt
}

// Penalty returns a peer's current points after decay.
func (s *Scorer) Penalty(p PeerID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.current(p, s.now()))
}

// evict frees a slot in a full map: drop entries that have decayed to 0, else
// evict the least-recently-seen one. Caller holds the lock.
func (s *Scorer) evict(now time.Time) {
	var oldestID PeerID
	var oldest time.Time
	for id, e := range s.scores {
		eff := math.Max(0, e.points-now.Sub(e.last).Seconds()*s.decay)
		if eff == 0 {
			delete(s.scores, id)
			continue
		}
		if oldestID == "" || e.last.Before(oldest) {
			oldestID, oldest = id, e.last
		}
	}
	if len(s.scores) >= s.maxTracked && oldestID != "" {
		delete(s.scores, oldestID)
	}
}
