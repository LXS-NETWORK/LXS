package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newHealer(now *time.Time, resync, redial func(context.Context) error) *Healer {
	return &Healer{
		Resync:           resync,
		Redial:           redial,
		StaleResyncAfter: 90 * time.Second,
		Cooldown:         60 * time.Second,
		Now:              func() time.Time { return *now },
		Log:              func(string, ...any) {},
	}
}

// TestHealerResyncsAStalledHead: an unhealthy snapshot with a stale head fires Resync —
// once — and again only after the cooldown elapses.
func TestHealerResyncsAStalledHead(t *testing.T) {
	now := t0
	var resyncs int
	h := newHealer(&now, func(context.Context) error { resyncs++; return nil }, nil)

	stalled := Snapshot{Status: StatusCritical, PeerCount: 3, HeadAgeSec: 120} // > 90s
	h.Handle(context.Background(), stalled)
	if resyncs != 1 {
		t.Fatalf("resyncs after first stall = %d, want 1", resyncs)
	}
	// within cooldown -> no second attempt.
	now = now.Add(30 * time.Second)
	h.Handle(context.Background(), stalled)
	if resyncs != 1 {
		t.Fatalf("resyncs during cooldown = %d, want 1 (throttled)", resyncs)
	}
	// past cooldown -> attempt again.
	now = now.Add(40 * time.Second) // total 70s > 60s cooldown
	h.Handle(context.Background(), stalled)
	if resyncs != 2 {
		t.Fatalf("resyncs after cooldown = %d, want 2", resyncs)
	}
}

// TestHealerRedialsWhenIsolated: no peers => Redial fires.
func TestHealerRedialsWhenIsolated(t *testing.T) {
	now := t0
	var redials int
	h := newHealer(&now, nil, func(context.Context) error { redials++; return nil })

	h.Handle(context.Background(), Snapshot{Status: StatusCritical, PeerCount: 0, HeadAgeSec: 5})
	if redials != 1 {
		t.Fatalf("redials when isolated = %d, want 1", redials)
	}
}

// TestHealerDoesNothingWhenHealthy: an OK snapshot triggers no remedy, even if some vital
// would otherwise match (defensive: OK means OK).
func TestHealerDoesNothingWhenHealthy(t *testing.T) {
	now := t0
	var resyncs, redials int
	h := newHealer(&now,
		func(context.Context) error { resyncs++; return nil },
		func(context.Context) error { redials++; return nil })
	h.Handle(context.Background(), Snapshot{Status: StatusOK, PeerCount: 0, HeadAgeSec: 9999})
	if resyncs != 0 || redials != 0 {
		t.Fatalf("healthy node triggered remedies: resyncs=%d redials=%d", resyncs, redials)
	}
}

// TestHealerHonestWhenUnwired: a needed remedy with no action wired does not panic and
// records no attempt (it logs that it cannot heal that dimension).
func TestHealerHonestWhenUnwired(t *testing.T) {
	now := t0
	h := newHealer(&now, nil, nil) // no remedies wired
	h.Handle(context.Background(), Snapshot{Status: StatusCritical, PeerCount: 0, HeadAgeSec: 120})
	if h.Counts("resync") != 0 || h.Counts("redial") != 0 {
		t.Fatalf("unwired healer recorded attempts: %d/%d", h.Counts("resync"), h.Counts("redial"))
	}
}

// TestHealerCountsFailedAttempts: a remedy that errors still counts as an attempt (so the
// cooldown applies) and does not panic.
func TestHealerCountsFailedAttempts(t *testing.T) {
	now := t0
	h := newHealer(&now, func(context.Context) error { return errors.New("peer refused") }, nil)
	h.Handle(context.Background(), Snapshot{Status: StatusCritical, PeerCount: 2, HeadAgeSec: 120})
	if h.Counts("resync") != 1 {
		t.Fatalf("failed resync attempt count = %d, want 1", h.Counts("resync"))
	}
}

// TestHealerIsolationDefersResync: when isolated AND stalled, only redial fires — resync
// is pointless without peers and must not burn its cooldown on a guaranteed failure. Once
// peers return, a still-stale head resyncs.
func TestHealerIsolationDefersResync(t *testing.T) {
	now := t0
	var resyncs, redials int
	h := newHealer(&now,
		func(context.Context) error { resyncs++; return nil },
		func(context.Context) error { redials++; return nil })
	// isolated + stale -> redial only
	h.Handle(context.Background(), Snapshot{Status: StatusCritical, PeerCount: 0, HeadAgeSec: 200})
	if redials != 1 || resyncs != 0 {
		t.Fatalf("isolated+stale: redials=%d resyncs=%d, want 1/0 (resync deferred)", redials, resyncs)
	}
	// peers came back, head still stale -> now resync (not on cooldown, since it never ran)
	h.Handle(context.Background(), Snapshot{Status: StatusCritical, PeerCount: 3, HeadAgeSec: 200})
	if resyncs != 1 {
		t.Fatalf("resync after peers returned = %d, want 1 (cooldown was not wasted while isolated)", resyncs)
	}
}

// TestHealerRelievesOnResourcePressure: low disk OR a goroutine leak fires Relieve; a
// healthy resource picture does not.
func TestHealerRelievesOnResourcePressure(t *testing.T) {
	now := t0
	var relieved int
	mk := func() *Healer {
		return &Healer{
			Relieve:  func(context.Context) error { relieved++; return nil },
			Cooldown: 60 * time.Second, DiskReliefPct: 8, GoroutineRelief: 40000,
			Now: func() time.Time { return now }, Log: func(string, ...any) {},
		}
	}
	// low disk
	relieved = 0
	mk().Handle(context.Background(), Snapshot{Status: StatusCritical, PeerCount: 3, DiskMonitored: true, DiskFreePct: 2, Goroutines: 10})
	if relieved != 1 {
		t.Fatalf("low disk relieved %d times, want 1", relieved)
	}
	// goroutine leak
	relieved = 0
	mk().Handle(context.Background(), Snapshot{Status: StatusDegraded, PeerCount: 3, DiskFreePct: 100, Goroutines: 90000})
	if relieved != 1 {
		t.Fatalf("goroutine leak relieved %d times, want 1", relieved)
	}
	// healthy resources -> no relief
	relieved = 0
	mk().Handle(context.Background(), Snapshot{Status: StatusDegraded, PeerCount: 3, DiskFreePct: 100, Goroutines: 10})
	if relieved != 0 {
		t.Fatalf("healthy resources relieved %d times, want 0", relieved)
	}
	// An unmonitored disk (DiskMonitored=false) with a zero DiskFreePct must not be read as
	// "disk 0% full" — the healer must ignore disk it is not actually measuring. (Deleting the
	// `cur.DiskMonitored &&` guard in Handle must fail this.)
	relieved = 0
	mk().Handle(context.Background(), Snapshot{Status: StatusDegraded, PeerCount: 3, DiskMonitored: false, DiskFreePct: 0, Goroutines: 10})
	if relieved != 0 {
		t.Fatalf("unmonitored disk (zero-value DiskFreePct) falsely relieved %d times, want 0", relieved)
	}
}

// TestHealerResyncDisabled: StaleResyncAfter == 0 means "never resync".
func TestHealerResyncDisabled(t *testing.T) {
	now := t0
	var resyncs int
	h := newHealer(&now, func(context.Context) error { resyncs++; return nil }, nil)
	h.StaleResyncAfter = 0
	h.Handle(context.Background(), Snapshot{Status: StatusCritical, PeerCount: 3, HeadAgeSec: 99999})
	if resyncs != 0 {
		t.Fatalf("resync fired with StaleResyncAfter=0: %d", resyncs)
	}
}

// TestHealerUnwiredLogThrottled: an unwired remedy needed every tick does not count as an
// attempt and is throttled by the cooldown (no per-tick spam).
func TestHealerUnwiredLogThrottled(t *testing.T) {
	now := t0
	logs := 0
	h := &Healer{StaleResyncAfter: 90 * time.Second, Cooldown: 60 * time.Second,
		Now: func() time.Time { return now }, Log: func(string, ...any) { logs++ }}
	stale := Snapshot{Status: StatusCritical, PeerCount: 3, HeadAgeSec: 120}
	h.Handle(context.Background(), stale)
	h.Handle(context.Background(), stale) // within cooldown — must not log again
	if logs != 1 {
		t.Fatalf("unwired remedy logged %d times, want 1 (throttled)", logs)
	}
	if h.Counts("resync") != 0 {
		t.Fatalf("unwired remedy recorded %d attempts, want 0", h.Counts("resync"))
	}
}
