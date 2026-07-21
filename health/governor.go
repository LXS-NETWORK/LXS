package health

import (
	"sync"
	"time"
)

// Governor applies a fixed rule set to make discrete policy decisions automatically, each
// bounded and logged. It executes rules configured up front; it never invents policy, can
// only land on a Posture one of its rules names, and records every change (from, to, why,
// when) so the decision trail is auditable. Same shape as the chain's difficulty
// adjustment: a parameter governed by rule.
//
// Postures are configured labels (e.g. "normal", "guarded", "lockdown"); the Apply callback
// turns a posture into concrete effect (e.g. how aggressively the warden sweeps). Apply is
// invoked only on a real change, not every tick.
type Posture string

// GovRule is one rule: if Match holds for the current snapshot, the node should be in
// Posture (Reason explains why, for the audit log). Rules are evaluated in order; the first
// match wins, so put the most severe rules first.
type GovRule struct {
	Posture Posture
	Reason  string
	Match   func(Snapshot) bool
}

// GovDecision is one audited governance change.
type GovDecision struct {
	Seq    int
	From   Posture
	To     Posture
	Reason string
	At     time.Time
}

// Governor evaluates its rule set against health snapshots and drives the node's Posture.
type Governor struct {
	Rules   []GovRule     // first match wins
	Default Posture       // posture when no rule matches
	Apply   func(Posture) // applied only on a change
	Now     func() time.Time
	Log     func(format string, args ...any)

	MaxAudit int // cap the retained audit log (0 => default)

	mu      sync.Mutex
	cur     Posture
	started bool
	audit   []GovDecision
	seq     int
}

func (g *Governor) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Evaluate picks the posture the rules dictate for s, applies it if it changed, records the
// decision, and returns it. Called every tick (wire to Reporter.OnTick alongside the
// healer). Pure w.r.t. s — it reads, it does not mutate the snapshot.
func (g *Governor) Evaluate(s Snapshot) Posture {
	target := g.Default
	reason := "no rule matched — default posture"
	for _, r := range g.Rules {
		if r.Match != nil && r.Match(s) {
			target, reason = r.Posture, r.Reason
			break
		}
	}

	g.mu.Lock()
	// The first evaluation establishes the baseline posture: record it (so the audit has an
	// origin) and apply it once.
	if !g.started {
		g.started = true
		g.cur = target
		g.record(g.cur, target, "initial posture: "+reason)
		g.mu.Unlock()
		if g.Apply != nil {
			g.Apply(target)
		}
		return target
	}
	if target == g.cur {
		g.mu.Unlock()
		return target
	}
	from := g.cur
	g.cur = target
	g.record(from, target, reason)
	g.mu.Unlock()

	if g.Log != nil {
		g.Log("governor: posture %s -> %s (%s)", from, target, reason)
	}
	if g.Apply != nil {
		g.Apply(target) // only on a real change
	}
	return target
}

// record appends an audit entry (caller holds the lock). Trims to MaxAudit.
func (g *Governor) record(from, to Posture, reason string) {
	g.seq++
	g.audit = append(g.audit, GovDecision{Seq: g.seq, From: from, To: to, Reason: reason, At: g.now()})
	max := g.MaxAudit
	if max <= 0 {
		max = 256
	}
	if len(g.audit) > max {
		g.audit = g.audit[len(g.audit)-max:]
	}
}

// Audit returns a copy of the decision log.
func (g *Governor) Audit() []GovDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]GovDecision, len(g.audit))
	copy(out, g.audit)
	return out
}
