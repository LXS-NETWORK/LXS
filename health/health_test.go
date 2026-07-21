package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fixed clock helpers so headAge is deterministic.
var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func base() *Monitor {
	return &Monitor{
		Height:     func() uint64 { return 100 },
		HeadTime:   func() time.Time { return t0 }, // head is "now" => fresh
		Mempool:    func() int { return 3 },
		Peers:      func() []PeerHealth { return []PeerHealth{{ID: "a"}, {ID: "b"}} },
		Restarts:   func() map[string]int64 { return map[string]int64{"task-a": 0} },
		Now:        func() time.Time { return t0 },
		StartedAt:  t0.Add(-2 * time.Hour),
		StaleAfter: 60 * time.Second, CriticalStale: 5 * time.Minute,
		RestartAlarm: 5, MempoolAlarm: 10000,
	}
}

func TestHealthyNodeIsOK(t *testing.T) {
	s := base().Snapshot()
	if s.Status != StatusOK {
		t.Fatalf("healthy node = %s (%v), want ok", s.Status, s.Reasons)
	}
	if s.PeerCount != 2 || s.Height != 100 {
		t.Fatalf("snapshot vitals wrong: peers=%d height=%d", s.PeerCount, s.Height)
	}
}

func TestNoPeersIsCritical(t *testing.T) {
	m := base()
	m.Peers = func() []PeerHealth { return nil }
	s := m.Snapshot()
	if s.Status != StatusCritical {
		t.Fatalf("isolated node = %s, want critical", s.Status)
	}
	if !hasReason(s, "isolated") {
		t.Fatalf("missing isolation reason: %v", s.Reasons)
	}
}

func TestStaleHeadDegradesThenCriticals(t *testing.T) {
	m := base()
	// 2 minutes old > 60s StaleAfter, < 5m CriticalStale => degraded.
	m.HeadTime = func() time.Time { return t0.Add(-2 * time.Minute) }
	if s := m.Snapshot(); s.Status != StatusDegraded {
		t.Fatalf("2m-old head = %s, want degraded", s.Status)
	}
	// 10 minutes old > CriticalStale => critical.
	m.HeadTime = func() time.Time { return t0.Add(-10 * time.Minute) }
	if s := m.Snapshot(); s.Status != StatusCritical {
		t.Fatalf("10m-old head = %s, want critical", s.Status)
	}
}

// TestColdStartOldGenesisIsNotStalled: a freshly-started node whose head is an ancient
// genesis must not report a stall — it has not waited longer than its own uptime.
// (Locks the fix for the false critical a bare miner booted into.)
func TestColdStartOldGenesisIsNotStalled(t *testing.T) {
	m := base()
	m.HeadTime = func() time.Time { return t0.Add(-3 * 365 * 24 * time.Hour) } // genesis from years ago
	m.StartedAt = t0.Add(-5 * time.Second)                                     // but just started
	s := m.Snapshot()
	if s.Status != StatusOK {
		t.Fatalf("cold-started node with old genesis = %s (%v), want ok", s.Status, s.Reasons)
	}
	if s.HeadAgeSec > 6 { // stall measured from start (~5s), not from the ancient head
		t.Fatalf("stall age = %.0fs, want ~5s (measured from start, not genesis)", s.HeadAgeSec)
	}
}

func TestCrashLoopingTaskDegrades(t *testing.T) {
	m := base()
	m.Restarts = func() map[string]int64 { return map[string]int64{"pawn-keeper": 7} } // >= 5 alarm
	s := m.Snapshot()
	if s.Status != StatusDegraded {
		t.Fatalf("crash-looping task = %s, want degraded", s.Status)
	}
	if !hasReason(s, "pawn-keeper") {
		t.Fatalf("missing crash-loop reason: %v", s.Reasons)
	}
}

func TestBannedPeerDegrades(t *testing.T) {
	m := base()
	m.Peers = func() []PeerHealth { return []PeerHealth{{ID: "a"}, {ID: "bad", Penalty: 60, Banned: true}} }
	s := m.Snapshot()
	if s.Status != StatusDegraded {
		t.Fatalf("node with a banned peer = %s, want degraded", s.Status)
	}
	if s.BannedPeers != 1 {
		t.Fatalf("bannedPeers = %d, want 1", s.BannedPeers)
	}
}

// TestEscalationTakesTheWorst: several problems at once => Status is the max severity, and
// every reason is recorded (so an operator sees the full picture, not just the worst).
func TestEscalationTakesTheWorst(t *testing.T) {
	m := base()
	m.HeadTime = func() time.Time { return t0.Add(-10 * time.Minute) }               // critical
	m.Restarts = func() map[string]int64 { return map[string]int64{"x": 9} }         // degraded
	m.Peers = func() []PeerHealth { return []PeerHealth{{ID: "bad", Banned: true}} } // degraded
	s := m.Snapshot()
	if s.Status != StatusCritical {
		t.Fatalf("worst-of-many = %s, want critical", s.Status)
	}
	if len(s.Reasons) < 3 {
		t.Fatalf("expected >=3 reasons, got %v", s.Reasons)
	}
}

// TestDiskDiagnosis: low free space degrades, nearly-full is critical, and an unmonitored
// disk never false-triggers.
func TestDiskDiagnosis(t *testing.T) {
	m := base()
	m.DiskLowPct, m.DiskCriticalPct = 10, 3
	// 8% free -> degraded
	m.DiskFree = func() (uint64, uint64) { return 8, 100 }
	if s := m.Snapshot(); s.Status != StatusDegraded || !hasReason(s, "disk") {
		t.Fatalf("8%% free = %s (%v), want degraded with a disk reason", s.Status, s.Reasons)
	}
	// 2% free -> critical
	m.DiskFree = func() (uint64, uint64) { return 2, 100 }
	if s := m.Snapshot(); s.Status != StatusCritical {
		t.Fatalf("2%% free = %s, want critical", s.Status)
	}
	// unmonitored disk (base has no DiskFree) never triggers
	if s := base().Snapshot(); s.DiskFreePct != 100 || hasReason(s, "disk") {
		t.Fatalf("unmonitored disk falsely reported: %v", s.Reasons)
	}
}

// TestStoreDiagnosis: an unreadable data store is critical (the node cannot function).
func TestStoreDiagnosis(t *testing.T) {
	m := base()
	m.StoreOK = func() error { return errors.New("pebble: corrupted manifest") }
	s := m.Snapshot()
	if s.Status != StatusCritical || !hasReason(s, "data store") {
		t.Fatalf("broken store = %s (%v), want critical", s.Status, s.Reasons)
	}
	// a healthy store does not flag.
	m.StoreOK = func() error { return nil }
	if s := m.Snapshot(); s.StoreErr != "" || s.Status != StatusOK {
		t.Fatalf("healthy store flagged: %s (%v)", s.Status, s.Reasons)
	}
}

// TestGoroutineLeakDiagnosis: an abnormal goroutine count degrades (leak signal).
func TestGoroutineLeakDiagnosis(t *testing.T) {
	m := base()
	m.GoroutineAlarm = 1000
	m.Goroutines = func() int { return 5000 }
	if s := m.Snapshot(); s.Status != StatusDegraded || !hasReason(s, "goroutines") {
		t.Fatalf("goroutine leak = %s (%v), want degraded", s.Status, s.Reasons)
	}
	m.Goroutines = func() int { return 50 }
	if s := m.Snapshot(); s.Status != StatusOK {
		t.Fatalf("normal goroutines flagged: %s", s.Status)
	}
}

func TestNilSeamsDoNotPanic(t *testing.T) {
	m := &Monitor{} // everything nil
	s := m.Snapshot()
	// No peers seam wired => isolation is not claimed (peers cannot be observed at all).
	if s.Status != StatusOK {
		t.Fatalf("bare monitor = %s, want ok (nothing observable)", s.Status)
	}
}

func hasReason(s Snapshot, substr string) bool {
	for _, r := range s.Reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

// TestReporterFiresOnChange: the OnChange seam (the healing trigger) fires exactly on a
// Status transition, with the previous and current status.
func TestReporterFiresOnChange(t *testing.T) {
	m := base()
	status := StatusOK
	m.Peers = func() []PeerHealth {
		if status == StatusCritical {
			return nil // flip to isolated
		}
		return []PeerHealth{{ID: "a"}}
	}

	var transitions []string
	r := &Reporter{
		Monitor: m,
		OnChange: func(prev, cur Snapshot) {
			transitions = append(transitions, string(prev.Status)+"->"+string(cur.Status))
		},
	}
	log := func(string, ...any) {}

	r.tick(context.Background(), log) // first reading: ok, establishes baseline (no announced change)
	status = StatusCritical
	r.tick(context.Background(), log) // ok -> critical
	status = StatusOK
	r.tick(context.Background(), log) // critical -> ok

	if len(transitions) != 2 || !strings.HasSuffix(transitions[0], "->critical") || !strings.HasSuffix(transitions[1], "->ok") {
		t.Fatalf("transitions = %v, want [*->critical *->ok]", transitions)
	}
}

// TestReporterOnTickFiresEveryTick: OnTick (the hook the healer and governor wire to) runs
// on every reading with the live snapshot, unlike OnChange which fires only on a transition.
func TestReporterOnTickFiresEveryTick(t *testing.T) {
	var ticks int
	var lastStatus Status
	r := &Reporter{
		Monitor: base(),
		OnTick:  func(ctx context.Context, s Snapshot) { ticks++; lastStatus = s.Status },
	}
	log := func(string, ...any) {}
	r.tick(context.Background(), log)
	r.tick(context.Background(), log)
	r.tick(context.Background(), log)
	if ticks != 3 {
		t.Fatalf("OnTick fired %d times, want 3 (every tick)", ticks)
	}
	if lastStatus != StatusOK {
		t.Fatalf("OnTick got status %s, want the live ok snapshot", lastStatus)
	}
}

// TestReporterRunStopsOnContext confirms Run returns promptly on cancel (clean shutdown).
func TestReporterRunStopsOnContext(t *testing.T) {
	r := &Reporter{Monitor: base(), Every: time.Hour, Log: func(string, ...any) {}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
}
