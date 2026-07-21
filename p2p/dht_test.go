//go:build libp2p

package p2p

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libp2pnet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestDHTPeerDiscoveryBeyondSeed is the proof that discovery is decentralized, not
// seed-bound. Three real libp2p nodes share a chain id. B and C each bootstrap ONLY
// to A and are never told the other's address. If they end up connected to EACH
// OTHER, the connection could only have come from the Kademlia DHT — the seed was
// the way in, not a permanent dependency. This is the failure mode the whole change
// exists to prevent: a network where killing the one seed isolates every node.
//
// The test forces DHTServerMode so a routing table forms on localhost. autonat
// cannot tell a 127.0.0.1 node is publicly reachable, so production ModeAuto would
// leave every localhost node a DHT client and no routing table would ever form —
// that is a test-harness limitation, not a production one, hence the knob.
func TestDHTPeerDiscoveryBeyondSeed(t *testing.T) {
	const chainID = 424242

	// A: the seed. Listens, advertises, holds the provider records B and C publish.
	a := newDHTNode(t, chainID, nil)
	seed := loopbackAddr(t, a)

	// B and C: each knows ONLY A. They do NOT know each other. Any B<->C connection
	// must be discovered through the DHT.
	b := newDHTNode(t, chainID, []string{seed})
	c := newDHTNode(t, chainID, []string{seed})

	bID, err := peer.Decode(string(b.Self()))
	if err != nil {
		t.Fatalf("decoding B id: %v", err)
	}
	cID, err := peer.Decode(string(c.Self()))
	if err != nil {
		t.Fatalf("decoding C id: %v", err)
	}

	// Poll, do not sleep a fixed amount: DHT convergence time varies with scheduling.
	// A generous ceiling keeps the test robust; it usually passes far sooner.
	deadline := time.After(60 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		bSeesC := b.host.Network().Connectedness(cID) == libp2pnet.Connected
		cSeesB := c.host.Network().Connectedness(bID) == libp2pnet.Connected
		if bSeesC && cSeesB {
			return // B and C found each other through the DHT: discovery is decentralized.
		}
		select {
		case <-deadline:
			t.Fatalf("B and C did not discover each other via the DHT within the timeout "+
				"(B<->C connected: B->C=%v C->B=%v) — discovery is still seed-bound",
				bSeesC, cSeesB)
		case <-tick.C:
		}
	}
}

// newDHTNode starts a real libp2p node on an ephemeral loopback port with the DHT in
// server mode, registered for cleanup. bootstrap is the seed list (nil for the seed
// itself).
func newDHTNode(t *testing.T, chainID uint64, bootstrap []string) *LibP2P {
	t.Helper()
	n, err := NewLibP2P(context.Background(), LibP2PConfig{
		ListenPort:    0, // OS picks a free port; no port collisions across the 3 nodes
		ChainID:       chainID,
		Bootstrap:     bootstrap,
		DHTServerMode: true, // force a routing table on localhost (see the test doc)
		DisableMDNS:   true, // all nodes are on 127.0.0.1: mDNS would connect B<->C and mask the DHT
	})
	if err != nil {
		t.Fatalf("starting node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n
}

// loopbackAddr returns one dialable /ip4/127.0.0.1 multiaddr for n. The host listens
// on 0.0.0.0, so Addrs also reports LAN addresses; the test pins the loopback one so
// a node dials the seed on the same machine, not across the network.
func loopbackAddr(t *testing.T, n *LibP2P) string {
	t.Helper()
	for _, a := range n.Addrs() {
		if strings.Contains(a, "/ip4/127.0.0.1/") {
			return a
		}
	}
	t.Fatalf("no loopback address for seed; have %v", n.Addrs())
	return ""
}

// TestPersistedPeersAreNotProtectedSeeds locks in the security fix for the
// reboot-surviving eclipse: a peer remembered from a prior run (peers.json) is an
// UNTRUSTED warm-start hint, so it must never be Protected as a "bootstrap" seed.
// If it were, a sybil that got into the live peer set once would return after every
// reboot as an un-trimmable, permanently re-dialed seed, and the persisted set could
// blow past the connmgr high-water and starve honest peers. Only operator-configured
// bootstrap peers earn Protect.
func TestPersistedPeersAreNotProtectedSeeds(t *testing.T) {
	const chainID = 515151

	a := newDHTNode(t, chainID, nil)
	aAddr := loopbackAddr(t, a)
	aID, err := peer.Decode(string(a.Self()))
	if err != nil {
		t.Fatalf("decoding A id: %v", err)
	}

	// A peers.json as if written by a prior run, listing A.
	peersFile := filepath.Join(t.TempDir(), "peers.json")
	data, _ := json.Marshal([]string{aAddr})
	if err := os.WriteFile(peersFile, data, 0o600); err != nil {
		t.Fatalf("writing peers file: %v", err)
	}

	// B loads A from PERSISTENCE (untrusted hint), not from configured bootstrap.
	b, err := NewLibP2P(context.Background(), LibP2PConfig{
		ChainID: chainID, PeersPath: peersFile, DHTServerMode: true, DisableMDNS: true,
	})
	if err != nil {
		t.Fatalf("starting B: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	if b.host.ConnManager().IsProtected(aID, "bootstrap") {
		t.Fatal("persisted peer was Protected as a bootstrap seed — reboot-surviving eclipse vector")
	}

	// Contrast: an OPERATOR-CONFIGURED bootstrap peer IS protected (trusted).
	c := newDHTNode(t, chainID, []string{aAddr})
	if !c.host.ConnManager().IsProtected(aID, "bootstrap") {
		t.Fatal("configured bootstrap peer was not protected — trusted seeds must survive trimming")
	}
}
