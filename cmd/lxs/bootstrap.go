package main

// DefaultBootstrapPeers are the network entry points compiled into every LXS node, so a
// fresh node with no -bootstrap flag still finds the network. Resilience by design:
//
//   - a direct /ip4 addr to the founding seed: works with zero DNS, the day-one path;
//   - the same seed via /dns4/seed.lxs.network: if the seed's IP ever moves, only the
//     DNS record changes and already-shipped binaries keep connecting — no re-release;
//   - a /dnsaddr DNS seed (Bitcoin-style): the TXT records at _dnsaddr.seed.lxs.network
//     list whatever nodes are alive right now, maintained by anyone who holds the
//     domain. This is what lets the network be joined long after the founder's single
//     server — or the founder — is gone.
//
// The two DNS entries are inert until the domain exists and its records are set; until
// then a node simply falls back to the direct /ip4 seed (an unresolvable seed is
// skipped, never fatal). Add more seeds here as independent operators appear — each
// extra reachable seed removes a little more single-point-of-failure. Discovery past
// the seeds is the DHT's job; these are only the way IN. -no-default-bootstrap drops
// them all, for isolated devnets and tests.
var DefaultBootstrapPeers = []string{
	"/ip4/79.72.25.166/tcp/30303/p2p/12D3KooWRSSSocSqWG978SWKpimQsti4WkmjytQX7g1qgJKnNzuA",
	"/dns4/seed.lxs.network/tcp/30303/p2p/12D3KooWRSSSocSqWG978SWKpimQsti4WkmjytQX7g1qgJKnNzuA",
	"/dnsaddr/seed.lxs.network",
}

// mergeBootstrap combines the built-in default seeds with any operator-supplied ones,
// de-duplicated and order-preserving (user entries first, then defaults). When
// noDefault is set the defaults are dropped entirely and only the user list remains.
func mergeBootstrap(user []string, noDefault bool) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(user)+len(DefaultBootstrapPeers))
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, a := range user {
		add(a)
	}
	if !noDefault {
		for _, a := range DefaultBootstrapPeers {
			add(a)
		}
	}
	return out
}
