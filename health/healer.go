package health

import (
	"context"
	"sync"
	"time"
)

// Healer turns the monitor's assessment into recovery actions. Wired as the Reporter's
// per-tick hook: on every reading where the node is unhealthy it tries the matching remedy,
// cooldown-throttled so it nudges rather than hammers.
//
// Remedies are pluggable func seams, each nil-safe: a nil seam means this node cannot heal
// that dimension, which the Healer logs rather than faking a fix. Two remedies to start,
// both grounded in real p2p seams:
//
//   - Resync: the head has gone stale -> catch up from peers.
//   - Redial: no peers (isolated) -> re-connect to the network.
//
// It acts only on concrete, observable conditions (peer count, stall age), never on the
// reason strings, so it stays decoupled from the monitor's wording and cannot misfire on a
// cosmetic message change.
type Healer struct {
	Resync func(context.Context) error // catch up from peers (stalled head)
	Redial func(context.Context) error // re-connect to the network (isolation)
	// Relieve eases local resource pressure — reclaim memory (GC + release to the OS) and,
	// if a cleanup is wired, free disk. Fires on low disk or a suspected goroutine leak.
	// Safe and local: GC/FreeOSMemory cannot corrupt state.
	Relieve func(context.Context) error

	StaleResyncAfter time.Duration // head stall age at/above which to resync (0 = never)
	DiskReliefPct    float64       // free% at/below which to Relieve (default 8)
	GoroutineRelief  int           // goroutines at/above which to Relieve (default 40000)
	Cooldown         time.Duration // minimum gap between two attempts of the SAME remedy
	Now              func() time.Time
	Log              func(format string, args ...any)

	mu     sync.Mutex
	last   map[string]time.Time
	counts map[string]int
}

func (h *Healer) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Healer) cooldown() time.Duration {
	if h.Cooldown <= 0 {
		return 30 * time.Second
	}
	return h.Cooldown
}

func (h *Healer) log(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

// Handle is the per-tick entry point (wire it to Reporter.OnTick). On an unhealthy
// snapshot it fires the matching remedy; on a healthy one it does nothing.
func (h *Healer) Handle(ctx context.Context, cur Snapshot) {
	if cur.Status == StatusOK {
		return
	}
	// Resource pressure first — it is local and matters regardless of network state. Fires
	// on low disk or a suspected goroutine leak; the wired Relieve reclaims what it safely
	// can (memory always; disk if a cleanup is wired).
	diskRelief := h.DiskReliefPct
	if diskRelief <= 0 {
		diskRelief = 8
	}
	gorRelief := h.GoroutineRelief
	if gorRelief <= 0 {
		gorRelief = 40000
	}
	if (cur.DiskMonitored && cur.DiskFreePct < diskRelief) || (gorRelief > 0 && cur.Goroutines >= gorRelief) {
		h.run(ctx, "relieve", h.Relieve)
	}
	// Isolation first — a node with no peers can neither resync nor make progress; getting
	// back on the network is the prerequisite for every other recovery. Resync is pointless
	// while isolated (and would waste its cooldown on a guaranteed "no peers" failure), so
	// return here and resync only once peers are present.
	if cur.PeerCount == 0 {
		h.run(ctx, "redial", h.Redial)
		return
	}
	if h.StaleResyncAfter > 0 && cur.HeadAgeSec >= h.StaleResyncAfter.Seconds() {
		h.run(ctx, "resync", h.Resync)
	}
}

// run fires one remedy, honouring the per-remedy cooldown and logging when a remedy is
// needed but not wired. The cooldown is checked before the wired/unwired split so even the
// "no action wired" message is throttled: an unwired remedy on a persistently-unhealthy
// node logs once per cooldown, not every tick.
func (h *Healer) run(ctx context.Context, name string, action func(context.Context) error) {
	now := h.now()
	h.mu.Lock()
	if h.last == nil {
		h.last = map[string]time.Time{}
		h.counts = map[string]int{}
	}
	if last, ok := h.last[name]; ok && now.Sub(last) < h.cooldown() {
		h.mu.Unlock()
		return // still cooling down — don't hammer (nor spam the unwired message)
	}
	h.last[name] = now
	if action == nil {
		h.mu.Unlock()
		h.log("healer: %s needed but no action wired — cannot self-heal this dimension", name)
		return
	}
	h.counts[name]++
	h.mu.Unlock()

	h.log("healer: %s — attempting recovery", name)
	if err := action(ctx); err != nil {
		h.log("healer: %s failed: %v", name, err)
		return
	}
	h.log("healer: %s attempted", name)
}

// Counts reports how many times a remedy was fired — for tests and health introspection.
func (h *Healer) Counts(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[name]
}
