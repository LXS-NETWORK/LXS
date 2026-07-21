// Package health assembles a node's self-assessment from what it can locally observe: its
// own vitals (head height/age, mempool, disk, goroutines, store reachability) plus what
// p2p exposes about connected peers (count, per-peer misbehaviour score, ban status). It
// is the input the healer, warden, and governor act on.
//
// It imports no core/p2p/node type, only plain function seams, so it can watch any of them
// without a dependency and tests can drive it with fakes. Node wiring fills the seams from
// bc.Head(), pool.Len(), restart counts, and the p2p peer set + Scorer. A node cannot see
// a peer's CPU or disk, only what p2p reveals over the wire, so peer health is exactly
// that: count plus each peer's score and ban status.
package health

import (
	"fmt"
	"sort"
	"time"
)

// Status is the node's assessed condition. Ordered by severity so escalation is a max().
type Status string

const (
	StatusOK       Status = "ok"
	StatusDegraded Status = "degraded"
	StatusCritical Status = "critical"
)

func severity(s Status) int {
	switch s {
	case StatusCritical:
		return 2
	case StatusDegraded:
		return 1
	default:
		return 0
	}
}

// PeerHealth is the observable view of one connected peer — only what p2p reveals.
type PeerHealth struct {
	ID      string `json:"id"`
	Penalty int    `json:"penalty"`
	Banned  bool   `json:"banned"`
}

// Snapshot is a point-in-time assessment: the node's own vitals, its view of its peers,
// and an overall Status with the concrete Reasons behind it.
type Snapshot struct {
	Status      Status           `json:"status"`
	Reasons     []string         `json:"reasons"`
	Height      uint64           `json:"height"`
	HeadAgeSec  float64          `json:"headAgeSec"`
	Mempool     int              `json:"mempool"`
	PeerCount   int              `json:"peerCount"`
	BannedPeers int              `json:"bannedPeers"`
	Peers       []PeerHealth     `json:"peers"`
	Restarts    map[string]int64 `json:"restarts"`
	UptimeSec   float64          `json:"uptimeSec"`
	Producing   bool             `json:"producing"`
	// Local resource/data diagnosis. DiskFreePct is 100 when disk is not monitored (so it
	// never false-triggers); Goroutines is 0 when not monitored; StoreErr is set only on a
	// real data-layer read failure.
	DiskFreePct   float64 `json:"diskFreePct"`
	DiskMonitored bool    `json:"diskMonitored"` // true only when disk is actually measured
	Goroutines    int     `json:"goroutines"`
	StoreErr      string  `json:"storeErr,omitempty"`
}

// Monitor assembles a Snapshot from function seams (no core/p2p import). Every seam is
// optional: a nil seam contributes nothing rather than panicking, so a node with no p2p
// still reports its local vitals.
type Monitor struct {
	Height   func() uint64           // current head height
	HeadTime func() time.Time        // head block's timestamp (caller converts the ms clock)
	Mempool  func() int              // pending-pool size
	Peers    func() []PeerHealth     // connected peers + their scores (the p2p view)
	Restarts func() map[string]int64 // supervised task -> restart count
	Now      func() time.Time

	// Local self-diagnosis seams. Each nil => that dimension is not checked.
	DiskFree   func() (freeBytes, totalBytes uint64) // datadir free space
	StoreOK    func() error                          // a lightweight data-layer read; non-nil => broken store
	Goroutines func() int                            // runtime goroutine count (a leak signal)

	StartedAt time.Time // process start, for uptime
	Producing bool      // is this node a miner (colours the stale-head reason)

	// Thresholds — all tunable; zero means "use the default".
	StaleAfter      time.Duration // head older than this => DEGRADED (falling behind)
	CriticalStale   time.Duration // head older than this => CRITICAL (chain/sync stalled)
	RestartAlarm    int64         // a task restarted >= this => DEGRADED (crash-looping)
	MempoolAlarm    int           // pool backlog >= this => DEGRADED
	DiskLowPct      float64       // free% below this => DEGRADED (default 10)
	DiskCriticalPct float64       // free% below this => CRITICAL (default 3)
	GoroutineAlarm  int           // goroutines >= this => DEGRADED (default 50000)
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Monitor) defaults() (staleAfter, criticalStale time.Duration, restartAlarm int64, mempoolAlarm int) {
	staleAfter, criticalStale, restartAlarm, mempoolAlarm = m.StaleAfter, m.CriticalStale, m.RestartAlarm, m.MempoolAlarm
	if staleAfter <= 0 {
		staleAfter = 60 * time.Second
	}
	if criticalStale <= 0 {
		criticalStale = 5 * time.Minute
	}
	if restartAlarm <= 0 {
		restartAlarm = 5
	}
	if mempoolAlarm <= 0 {
		mempoolAlarm = 10000
	}
	return
}

// Snapshot gathers the vitals and assesses them. It does not block and does not panic on a
// nil seam — the monitor must not itself be a source of instability.
func (m *Monitor) Snapshot() Snapshot {
	staleAfter, criticalStale, restartAlarm, mempoolAlarm := m.defaults()
	now := m.now()

	s := Snapshot{Status: StatusOK, Producing: m.Producing, Restarts: map[string]int64{}, DiskFreePct: 100}
	if m.Height != nil {
		s.Height = m.Height()
	}
	if m.Mempool != nil {
		s.Mempool = m.Mempool()
	}
	if m.DiskFree != nil {
		s.DiskMonitored = true
		if free, total := m.DiskFree(); total > 0 {
			s.DiskFreePct = float64(free) / float64(total) * 100
		}
	}
	if m.Goroutines != nil {
		s.Goroutines = m.Goroutines()
	}
	if m.StoreOK != nil {
		if err := m.StoreOK(); err != nil {
			s.StoreErr = err.Error()
		}
	}
	if m.Peers != nil {
		s.Peers = m.Peers()
	}
	if m.Restarts != nil {
		s.Restarts = m.Restarts()
	}
	if !m.StartedAt.IsZero() {
		s.UptimeSec = now.Sub(m.StartedAt).Seconds()
	}
	s.PeerCount = len(s.Peers)
	for _, p := range s.Peers {
		if p.Banned {
			s.BannedPeers++
		}
	}

	// Stall age: how long the head has gone without advancing. A stalled head means the node
	// stopped producing or lost sync — a problem for miners and followers alike. It is
	// measured from the later of the head's timestamp and process start: at cold start the
	// head is genesis (an ancient timestamp), but the node has not waited longer than its own
	// uptime, so a stall is only claimed once StaleAfter has elapsed since start. Otherwise
	// every fresh node would boot straight into a false critical.
	var headAge time.Duration
	if m.HeadTime != nil {
		ref := m.HeadTime()
		if m.StartedAt.After(ref) {
			ref = m.StartedAt
		}
		headAge = now.Sub(ref)
		if headAge < 0 {
			headAge = 0
		}
		s.HeadAgeSec = headAge.Seconds()
	}

	raise := func(to Status, reason string) {
		if severity(to) > severity(s.Status) {
			s.Status = to
		}
		s.Reasons = append(s.Reasons, reason)
	}

	// isolation is the most acute failure — a node with no peers is off the network.
	if m.Peers != nil && s.PeerCount == 0 {
		raise(StatusCritical, "no peers — isolated from the network")
	}
	if m.HeadTime != nil {
		switch {
		case headAge >= criticalStale:
			raise(StatusCritical, fmt.Sprintf("no new block for %s — chain/sync stalled", headAge.Round(time.Second)))
		case headAge >= staleAfter:
			what := "falling behind the network"
			if m.Producing {
				what = "not producing"
			}
			raise(StatusDegraded, fmt.Sprintf("head is %s old — %s", headAge.Round(time.Second), what))
		}
	}
	// a crash-looping supervised loop.
	names := make([]string, 0, len(s.Restarts))
	for n := range s.Restarts {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic reason order
	for _, n := range names {
		if s.Restarts[n] >= restartAlarm {
			raise(StatusDegraded, fmt.Sprintf("task %q restarted %d times — crash-looping", n, s.Restarts[n]))
		}
	}
	if m.Mempool != nil && s.Mempool >= mempoolAlarm {
		raise(StatusDegraded, fmt.Sprintf("mempool backlog %d — not clearing", s.Mempool))
	}
	// misbehaving peers.
	if s.BannedPeers > 0 {
		raise(StatusDegraded, fmt.Sprintf("%d of %d peers banned for misbehaviour", s.BannedPeers, s.PeerCount))
	}
	// local resources and data layer.
	diskLow, diskCrit := m.DiskLowPct, m.DiskCriticalPct
	if diskLow <= 0 {
		diskLow = 10
	}
	if diskCrit <= 0 {
		diskCrit = 3
	}
	if s.StoreErr != "" {
		raise(StatusCritical, "data store unreadable: "+s.StoreErr)
	}
	if m.DiskFree != nil {
		switch {
		case s.DiskFreePct < diskCrit:
			raise(StatusCritical, fmt.Sprintf("disk %.1f%% free — nearly full", s.DiskFreePct))
		case s.DiskFreePct < diskLow:
			raise(StatusDegraded, fmt.Sprintf("disk %.1f%% free — low", s.DiskFreePct))
		}
	}
	gorAlarm := m.GoroutineAlarm
	if gorAlarm <= 0 {
		gorAlarm = 50000
	}
	if m.Goroutines != nil && s.Goroutines >= gorAlarm {
		raise(StatusDegraded, fmt.Sprintf("%d goroutines — leak suspected", s.Goroutines))
	}

	if s.Reasons == nil {
		s.Reasons = []string{"all systems nominal"}
	}
	return s
}
