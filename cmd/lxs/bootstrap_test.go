package main

import (
	"strings"
	"testing"
)

// TestMergeBootstrapAddsDefaults: a node with no -bootstrap still gets the built-in
// seeds, so it can find the network out of the box.
func TestMergeBootstrapAddsDefaults(t *testing.T) {
	got := mergeBootstrap(nil, false)
	if len(got) != len(DefaultBootstrapPeers) {
		t.Fatalf("want %d default seeds, got %d", len(DefaultBootstrapPeers), len(got))
	}
	if got[0] != DefaultBootstrapPeers[0] {
		t.Fatalf("first seed = %q, want %q", got[0], DefaultBootstrapPeers[0])
	}
}

// TestMergeBootstrapUserFirstThenDefaults: operator seeds are kept AND the defaults are
// still added underneath (resilience is not lost by naming your own peer).
func TestMergeBootstrapUserFirstThenDefaults(t *testing.T) {
	user := []string{"/ip4/10.0.0.1/tcp/30303/p2p/12D3KooWtest"}
	got := mergeBootstrap(user, false)
	if got[0] != user[0] {
		t.Fatalf("user seed should come first, got %q", got[0])
	}
	if len(got) != len(user)+len(DefaultBootstrapPeers) {
		t.Fatalf("want user+defaults=%d, got %d", len(user)+len(DefaultBootstrapPeers), len(got))
	}
}

// TestMergeBootstrapNoDefault: -no-default-bootstrap drops the built-ins entirely, so
// a devnet/test node never reaches out to the real network's seeds.
func TestMergeBootstrapNoDefault(t *testing.T) {
	user := []string{"/ip4/10.0.0.1/tcp/30303/p2p/12D3KooWtest"}
	got := mergeBootstrap(user, true)
	if len(got) != 1 || got[0] != user[0] {
		t.Fatalf("no-default should yield only the user seed, got %v", got)
	}
	for _, a := range got {
		if strings.Contains(a, "seed.lxs.network") || strings.Contains(a, "79.72.25.166") {
			t.Fatalf("a default seed leaked through no-default: %q", a)
		}
	}
}

// TestMergeBootstrapDedupes: a user entry equal to a default is not listed twice.
func TestMergeBootstrapDedupes(t *testing.T) {
	dup := DefaultBootstrapPeers[0]
	got := mergeBootstrap([]string{dup}, false)
	n := 0
	for _, a := range got {
		if a == dup {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("duplicate seed appears %d times, want 1", n)
	}
	if len(got) != len(DefaultBootstrapPeers) {
		t.Fatalf("dedupe changed total: got %d, want %d", len(got), len(DefaultBootstrapPeers))
	}
}
