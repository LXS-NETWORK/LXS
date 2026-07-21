//go:build libp2p

package p2p

import (
	"context"
	"testing"

	madns "github.com/multiformats/go-multiaddr-dns"
)

const testSeedPeerID = "12D3KooWRSSSocSqWG978SWKpimQsti4WkmjytQX7g1qgJKnNzuA"

// TestResolveBootstrapDirectAddr: a concrete /ip4 seed parses to exactly one AddrInfo.
func TestResolveBootstrapDirectAddr(t *testing.T) {
	infos := resolveBootstrap(context.Background(),
		"/ip4/79.72.25.166/tcp/30303/p2p/"+testSeedPeerID)
	if len(infos) != 1 {
		t.Fatalf("want 1 AddrInfo, got %d", len(infos))
	}
	if infos[0].ID.String() != testSeedPeerID {
		t.Fatalf("wrong peer id: %s", infos[0].ID)
	}
	if len(infos[0].Addrs) != 1 {
		t.Fatalf("want 1 dial addr, got %d", len(infos[0].Addrs))
	}
}

// TestResolveBootstrapDNSSeedExpandsToPeers proves the DNS-seed path: a /dnsaddr entry
// is resolved through its TXT records into the concrete peers it advertises. This is
// the mechanism that lets the entry point be maintained in DNS, outliving any one node.
// A deterministic mock resolver stands in for live DNS.
func TestResolveBootstrapDNSSeedExpandsToPeers(t *testing.T) {
	mock := &madns.MockResolver{TXT: map[string][]string{
		"_dnsaddr.seed.lxs.test": {
			"dnsaddr=/ip4/203.0.113.9/tcp/30303/p2p/" + testSeedPeerID,
		},
	}}
	res, err := madns.NewResolver(madns.WithDomainResolver("lxs.test", mock))
	if err != nil {
		t.Fatalf("building mock resolver: %v", err)
	}
	old := dnsResolver
	dnsResolver = res
	defer func() { dnsResolver = old }()

	infos := resolveBootstrap(context.Background(), "/dnsaddr/seed.lxs.test")
	if len(infos) != 1 {
		t.Fatalf("DNS seed should expand to 1 peer, got %d", len(infos))
	}
	if infos[0].ID.String() != testSeedPeerID {
		t.Fatalf("resolved wrong peer id: %s", infos[0].ID)
	}
}

// TestResolveBootstrapUnresolvableDNSSeedIsSkipped locks in the non-fatal contract: a
// DNS seed that does not resolve (a reserved .invalid name that RFC 6761 guarantees
// never resolves) yields nothing and does NOT error — a seed that is not set up yet
// can never stop a node from starting on the seeds that do work.
func TestResolveBootstrapUnresolvableDNSSeedIsSkipped(t *testing.T) {
	if infos := resolveBootstrap(context.Background(), "/dnsaddr/seed.lxs.invalid"); len(infos) != 0 {
		t.Fatalf("unresolvable DNS seed must yield 0 AddrInfos, got %d", len(infos))
	}
}

// TestResolveBootstrapGarbageIsSkipped: a non-multiaddr string is skipped, not fatal.
func TestResolveBootstrapGarbageIsSkipped(t *testing.T) {
	if infos := resolveBootstrap(context.Background(), "not-a-multiaddr"); len(infos) != 0 {
		t.Fatalf("garbage addr must yield 0, got %d", len(infos))
	}
}
