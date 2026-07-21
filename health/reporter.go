package health

import (
	"context"
	"strings"
	"time"
)

// Reporter is the monitoring loop: every tick it takes a Snapshot, logs it, and fires
// OnChange when the assessed Status changes.
//
// OnChange is the seam the Healer plugs into: a Status transition to critical is the trigger
// a remedy reacts to (resync, restart a stuck loop, re-dial peers). Optional; nil is a no-op.
type Reporter struct {
	Monitor  *Monitor
	Every    time.Duration
	Log      func(format string, args ...any)
	OnChange func(prev, cur Snapshot)                // fired when Status transitions; nil = no-op
	OnTick   func(ctx context.Context, cur Snapshot) // fired every tick; the healing hook

	last Status
}

// Run ticks until ctx is cancelled. A nil Monitor makes Run a clean no-op loop rather
// than a panic — an unwired monitor must not crash the node it is meant to watch.
func (r *Reporter) Run(ctx context.Context) error {
	if r.Every <= 0 {
		r.Every = 30 * time.Second
	}
	log := r.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	t := time.NewTicker(r.Every)
	defer t.Stop()

	// An initial reading right away, so the first health line does not wait a full tick.
	r.tick(ctx, log)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.tick(ctx, log)
		}
	}
}

func (r *Reporter) tick(ctx context.Context, log func(string, ...any)) {
	if r.Monitor == nil {
		return
	}
	s := r.Monitor.Snapshot()
	log("health: %s h=%d headAge=%.0fs peers=%d(banned=%d) mempool=%d up=%.0fs — %s",
		strings.ToUpper(string(s.Status)), s.Height, s.HeadAgeSec, s.PeerCount, s.BannedPeers,
		s.Mempool, s.UptimeSec, strings.Join(s.Reasons, "; "))

	// The healing hook runs every tick with the live snapshot: the Healer decides, under its
	// own cooldowns, whether the current state warrants a remedy.
	if r.OnTick != nil {
		r.OnTick(ctx, s)
	}

	if s.Status != r.last {
		// The first reading only establishes the baseline — not a transition from a known
		// state — so neither the log line nor the OnChange trigger fires on it. Both fire
		// only on a genuine change thereafter.
		if r.last != "" {
			log("health: status %s -> %s", strings.ToUpper(string(r.last)), strings.ToUpper(string(s.Status)))
			if r.OnChange != nil {
				r.OnChange(Snapshot{Status: r.last}, s)
			}
		}
		r.last = s.Status
	}
}
