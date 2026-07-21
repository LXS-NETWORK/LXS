package health

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestWardenDisconnectsOnlyBannedPeers: a sweep cuts the banned peers and leaves the
// honest ones connected.
func TestWardenDisconnectsOnlyBannedPeers(t *testing.T) {
	peers := []PeerHealth{
		{ID: "good1"},
		{ID: "bad1", Penalty: 60, Banned: true},
		{ID: "good2", Penalty: 3},
		{ID: "bad2", Banned: true},
	}
	var cut []string
	w := &PeerWarden{
		Peers:      func() []PeerHealth { return peers },
		Disconnect: func(id string) error { cut = append(cut, id); return nil },
	}
	if n := w.sweep(); n != 2 {
		t.Fatalf("sweep acted on %d peers, want 2", n)
	}
	if len(cut) != 2 || cut[0] != "bad1" || cut[1] != "bad2" {
		t.Fatalf("cut = %v, want [bad1 bad2]", cut)
	}
	if w.Enforced() != 2 {
		t.Fatalf("enforced = %d, want 2", w.Enforced())
	}
}

// TestWardenErrorDoesNotAbortSweep: a Disconnect failure on one peer still lets the sweep
// reach the others.
func TestWardenErrorDoesNotAbortSweep(t *testing.T) {
	peers := []PeerHealth{{ID: "bad1", Banned: true}, {ID: "bad2", Banned: true}}
	var reached []string
	w := &PeerWarden{
		Peers: func() []PeerHealth { return peers },
		Disconnect: func(id string) error {
			reached = append(reached, id)
			if id == "bad1" {
				return errors.New("already gone")
			}
			return nil
		},
	}
	if n := w.sweep(); n != 1 { // bad1 errored, bad2 succeeded
		t.Fatalf("sweep counted %d successful, want 1", n)
	}
	if len(reached) != 2 {
		t.Fatalf("sweep reached %v, want both peers attempted", reached)
	}
}

// TestWardenUnwiredIsNoop: no seams => no panic, no action.
func TestWardenUnwiredIsNoop(t *testing.T) {
	w := &PeerWarden{}
	if n := w.sweep(); n != 0 {
		t.Fatalf("unwired sweep acted on %d, want 0", n)
	}
}

// TestWardenGovernedInterval: interval() reads the dynamic Interval seam and falls back to
// Every, then to a default.
func TestWardenGovernedInterval(t *testing.T) {
	var ns int64 = int64(20 * time.Second)
	w := &PeerWarden{Interval: func() time.Duration { return time.Duration(atomic.LoadInt64(&ns)) }}
	if w.interval() != 20*time.Second {
		t.Fatalf("governed interval = %s, want 20s", w.interval())
	}
	atomic.StoreInt64(&ns, int64(3*time.Second)) // governor tightens to lockdown
	if w.interval() != 3*time.Second {
		t.Fatalf("after tighten interval = %s, want 3s", w.interval())
	}
	// Interval seam returning 0 falls back to Every.
	w0 := &PeerWarden{Every: 15 * time.Second, Interval: func() time.Duration { return 0 }}
	if w0.interval() != 15*time.Second {
		t.Fatalf("fallback interval = %s, want Every=15s", w0.interval())
	}
	// nothing set => a sane default.
	if (&PeerWarden{}).interval() <= 0 {
		t.Fatal("default interval must be positive")
	}
}

// TestWardenPokeTriggersImmediateSweep: a poke wakes the warden for a sweep now, without
// waiting out its (here very long) interval.
func TestWardenPokeTriggersImmediateSweep(t *testing.T) {
	cut := make(chan string, 4)
	poke := make(chan struct{}, 1)
	w := &PeerWarden{
		Peers:      func() []PeerHealth { return []PeerHealth{{ID: "bad", Banned: true}} },
		Disconnect: func(id string) error { cut <- id; return nil },
		Interval:   func() time.Duration { return time.Hour }, // only a poke can wake it in time
		Poke:       poke,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// the initial sweep fires on Run start.
	select {
	case <-cut:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial sweep")
	}
	// a poke fires another sweep promptly (not after the 1h interval).
	poke <- struct{}{}
	select {
	case <-cut:
	case <-time.After(2 * time.Second):
		t.Fatal("poke did not trigger a prompt sweep")
	}
}

// TestWardenAllHonestNoAction: an all-honest peer set triggers nothing.
func TestWardenAllHonestNoAction(t *testing.T) {
	peers := []PeerHealth{{ID: "a"}, {ID: "b", Penalty: 10}}
	var cut int
	w := &PeerWarden{
		Peers:      func() []PeerHealth { return peers },
		Disconnect: func(string) error { cut++; return nil },
	}
	if n := w.sweep(); n != 0 || cut != 0 {
		t.Fatalf("honest peer set: acted=%d cut=%d, want 0/0", n, cut)
	}
}
