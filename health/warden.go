package health

import (
	"context"
	"sync"
	"time"
)

// PeerWarden disconnects banned peers. The p2p Scorer penalises bad peers and marks them
// banned, which drops their messages before decoding, but a banned peer stays connected —
// holding a slot and, if it is a bootstrap peer, getting re-dialled. The warden periodically
// disconnects every banned peer. The ban remains the security boundary (traffic is ignored
// regardless); the disconnect frees the slot.
//
// Consensus rejects invalid blocks, the Scorer detects a peer that keeps sending them, and
// the warden enforces the verdict at the connection. Decoupled by func seams like the rest
// of the package: nil seams make it a clean no-op rather than a panic.
type PeerWarden struct {
	Peers      func() []PeerHealth   // current peers + ban status (the p2p view)
	Disconnect func(id string) error // cut a peer's connection
	Every      time.Duration
	// Interval, if set, governs the sweep cadence dynamically: the Governor tightens it under
	// a defensive posture and relaxes it when calm. Nil => fixed Every.
	Interval func() time.Duration
	// Poke, if set, wakes the warden for an immediate sweep + interval re-read: the Governor
	// signals it on a posture change so a tightened cadence takes effect at once instead of
	// after the current (possibly relaxed) sleep. Best-effort; a missed poke just waits for
	// the next interval.
	Poke <-chan struct{}
	Log  func(format string, args ...any)

	mu       sync.Mutex
	enforced int
}

func (w *PeerWarden) interval() time.Duration {
	if w.Interval != nil {
		if d := w.Interval(); d > 0 {
			return d
		}
	}
	if w.Every > 0 {
		return w.Every
	}
	return 20 * time.Second
}

func (w *PeerWarden) log(format string, args ...any) {
	if w.Log != nil {
		w.Log(format, args...)
	}
}

// Run sweeps for banned peers on a (possibly governed) cadence until ctx is cancelled.
func (w *PeerWarden) Run(ctx context.Context) error {
	w.sweep() // act immediately, don't wait a full tick to cut a known-bad peer
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.interval()):
			w.sweep()
		case <-w.Poke:
			w.sweep() // governed posture changed — sweep now at the new cadence
		}
	}
}

// sweep disconnects every currently-banned peer and returns how many it acted on. A
// Disconnect error on one peer is logged but does not abort the sweep — the other bad
// peers still get cut. Idempotent: a peer already gone simply is not in the next list.
func (w *PeerWarden) sweep() int {
	if w.Peers == nil || w.Disconnect == nil {
		return 0
	}
	acted := 0
	for _, p := range w.Peers() {
		if !p.Banned {
			continue
		}
		if err := w.Disconnect(p.ID); err != nil {
			w.log("warden: failed to disconnect banned peer %s: %v", p.ID, err)
			continue
		}
		acted++
		w.log("warden: disconnected banned peer %s (misbehaviour enforced)", p.ID)
	}
	if acted > 0 {
		w.mu.Lock()
		w.enforced += acted
		w.mu.Unlock()
	}
	return acted
}

// Enforced is the lifetime count of banned-peer disconnects — for health introspection.
func (w *PeerWarden) Enforced() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enforced
}
