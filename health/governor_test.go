package health

import (
	"sync/atomic"
	"testing"
	"time"
)

// defenseRules is a representative policy: lockdown when many peers are banned (an attack),
// guarded on any degraded/critical, else normal.
func defenseRules() []GovRule {
	return []GovRule{
		{Posture: "lockdown", Reason: "peers under coordinated attack", Match: func(s Snapshot) bool {
			return s.PeerCount > 0 && s.BannedPeers*2 >= s.PeerCount
		}},
		{Posture: "guarded", Reason: "node not fully healthy", Match: func(s Snapshot) bool {
			return s.Status != StatusOK
		}},
	}
}

func newGov(now *time.Time, applied *[]Posture) *Governor {
	return &Governor{
		Rules:   defenseRules(),
		Default: "normal",
		Apply:   func(p Posture) { *applied = append(*applied, p) },
		Now:     func() time.Time { return *now },
		Log:     func(string, ...any) {},
	}
}

// TestGovernorDrivesPostureByRules: the posture tracks the rules, and Apply fires only on
// real changes (not every tick).
func TestGovernorDrivesPostureByRules(t *testing.T) {
	now := t0
	var applied []Posture
	g := newGov(&now, &applied)

	healthy := Snapshot{Status: StatusOK, PeerCount: 4, BannedPeers: 0}
	degraded := Snapshot{Status: StatusDegraded, PeerCount: 4, BannedPeers: 1}
	attacked := Snapshot{Status: StatusDegraded, PeerCount: 4, BannedPeers: 2}

	if p := g.Evaluate(healthy); p != "normal" {
		t.Fatalf("healthy posture = %s, want normal", p)
	}
	g.Evaluate(healthy) // no change — Apply must not fire again
	if p := g.Evaluate(degraded); p != "guarded" {
		t.Fatalf("degraded posture = %s, want guarded", p)
	}
	if p := g.Evaluate(attacked); p != "lockdown" {
		t.Fatalf("attacked posture = %s, want lockdown", p)
	}
	if p := g.Evaluate(healthy); p != "normal" {
		t.Fatalf("recovered posture = %s, want normal", p)
	}
	// Apply fired for: initial normal, guarded, lockdown, normal = 4 (the repeat healthy did not).
	want := []Posture{"normal", "guarded", "lockdown", "normal"}
	if len(applied) != len(want) {
		t.Fatalf("apply sequence = %v, want %v", applied, want)
	}
	for i := range want {
		if applied[i] != want[i] {
			t.Fatalf("apply[%d] = %s, want %s (full %v)", i, applied[i], want[i], applied)
		}
	}
}

// TestGovernorAuditTrail: every change is recorded with from/to/reason, in order.
func TestGovernorAuditTrail(t *testing.T) {
	now := t0
	var applied []Posture
	g := newGov(&now, &applied)
	g.Evaluate(Snapshot{Status: StatusOK, PeerCount: 3})                       // normal (initial)
	g.Evaluate(Snapshot{Status: StatusCritical, PeerCount: 3, BannedPeers: 0}) // guarded
	g.Evaluate(Snapshot{Status: StatusCritical, PeerCount: 3, BannedPeers: 3}) // lockdown

	audit := g.Audit()
	if len(audit) != 3 {
		t.Fatalf("audit has %d entries, want 3", len(audit))
	}
	if audit[0].To != "normal" || audit[1].To != "guarded" || audit[2].To != "lockdown" {
		t.Fatalf("audit trail = %v, want normal->guarded->lockdown", audit)
	}
	if audit[1].From != "normal" || audit[2].From != "guarded" {
		t.Fatalf("audit 'from' links wrong: %v", audit)
	}
	if audit[1].Seq != 2 || audit[2].Seq != 3 {
		t.Fatalf("audit seq not monotonic: %v", audit)
	}
}

// TestGovernorStaysWithinDefinedPostures: with no matching rule it lands on the default and
// only ever emits postures named by its rules or its default.
func TestGovernorStaysWithinDefinedPostures(t *testing.T) {
	now := t0
	g := &Governor{
		Rules:   defenseRules(),
		Default: "normal",
		Now:     func() time.Time { return now },
	}
	allowed := map[Posture]bool{"normal": true, "guarded": true, "lockdown": true}
	inputs := []Snapshot{
		{Status: StatusOK, PeerCount: 2},
		{Status: StatusDegraded, PeerCount: 2, BannedPeers: 1},
		{Status: StatusCritical, PeerCount: 2, BannedPeers: 2},
		{Status: StatusOK, PeerCount: 0},
	}
	for _, s := range inputs {
		if p := g.Evaluate(s); !allowed[p] {
			t.Fatalf("governor emitted undefined posture %q", p)
		}
	}
}

// TestGovernorSkipsNilMatchRule: a rule with a nil Match is skipped, not treated as a
// match (guarding the governor.go nil-Match branch).
func TestGovernorSkipsNilMatchRule(t *testing.T) {
	now := t0
	g := &Governor{
		Rules: []GovRule{
			{Posture: "x", Match: nil},                                   // must be skipped
			{Posture: "y", Match: func(s Snapshot) bool { return true }}, // this should win
		},
		Default: "d",
		Now:     func() time.Time { return now },
	}
	if p := g.Evaluate(Snapshot{}); p != "y" {
		t.Fatalf("nil-Match rule not skipped: posture = %s, want y", p)
	}
}

// TestGovernorLeverDrivesWardenInterval: end to end, moving the posture to lockdown tightens
// the value the warden's interval() reads.
func TestGovernorLeverDrivesWardenInterval(t *testing.T) {
	now := t0
	var ns int64 = int64(20 * time.Second)
	postureIv := map[Posture]time.Duration{"normal": 20 * time.Second, "guarded": 8 * time.Second, "lockdown": 3 * time.Second}
	g := &Governor{
		Rules:   defenseRules(),
		Default: "normal",
		Apply: func(p Posture) {
			if d, ok := postureIv[p]; ok {
				atomic.StoreInt64(&ns, int64(d))
			}
		},
		Now: func() time.Time { return now },
	}
	w := &PeerWarden{Interval: func() time.Duration { return time.Duration(atomic.LoadInt64(&ns)) }}

	g.Evaluate(Snapshot{Status: StatusOK, PeerCount: 4}) // normal
	if w.interval() != 20*time.Second {
		t.Fatalf("normal posture warden interval = %s, want 20s", w.interval())
	}
	g.Evaluate(Snapshot{Status: StatusDegraded, PeerCount: 4, BannedPeers: 3}) // lockdown
	if w.interval() != 3*time.Second {
		t.Fatalf("lockdown posture warden interval = %s, want 3s (lever did not tighten)", w.interval())
	}
}

// TestGovernorAuditCap bounds the retained log.
func TestGovernorAuditCap(t *testing.T) {
	now := t0
	g := &Governor{
		Rules:    []GovRule{{Posture: "a", Match: func(s Snapshot) bool { return s.Height%2 == 0 }}},
		Default:  "b",
		Now:      func() time.Time { return now },
		MaxAudit: 4,
	}
	for i := uint64(0); i < 50; i++ {
		g.Evaluate(Snapshot{Height: i}) // alternates a/b every step -> many changes
	}
	if len(g.Audit()) > 4 {
		t.Fatalf("audit not capped: %d entries, want <= 4", len(g.Audit()))
	}
}
