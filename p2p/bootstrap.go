//go:build libp2p

package p2p

import (
	"context"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"
)

// dnsResolver resolves DNS-based multiaddrs (/dnsaddr). It is a package var so a test
// can substitute a deterministic mock for the live network resolver.
var dnsResolver = madns.DefaultResolver

// bootstrapResolveTimeout bounds a single DNS-seed lookup. A seed that hangs must not
// hold up node startup; the direct hardcoded seeds are already dialable meanwhile.
const bootstrapResolveTimeout = 10 * time.Second

// resolveBootstrap turns one bootstrap entry into zero or more concrete peer AddrInfos.
//
// A /dnsaddr/<host> entry is a DNS SEED in the Bitcoin sense: the TXT records at
// _dnsaddr.<host> list the full multiaddrs of whatever nodes are alive right now, each
// carrying its own peer id. That list lives in DNS — editable by whoever holds the
// domain, not baked into one binary and not tied to the founder's one server or
// lifetime — so the way INTO the network can be re-pointed long after any single node,
// or person, is gone. We resolve it here so each currently-advertised node becomes a
// usable seed.
//
// Everything else (a direct /ip4 or /dns4 .../p2p/<id> address) is parsed as-is. A
// /dns4 host keeps its inline peer id and is left for the swarm to resolve at DIAL
// time, so it always dials the current IP behind the name.
//
// Resolution or parse failure is deliberately NON-FATAL and yields nothing: a DNS seed
// that is not set up yet, or a stale hardcoded entry, must never stop a node from
// starting on the seeds that DO work.
func resolveBootstrap(ctx context.Context, addr string) []peer.AddrInfo {
	ma, err := multiaddr.NewMultiaddr(addr)
	if err != nil {
		log.Printf("p2p: ignoring bad bootstrap addr %q: %v", addr, err)
		return nil
	}
	if hasDnsaddr(ma) {
		rctx, cancel := context.WithTimeout(ctx, bootstrapResolveTimeout)
		defer cancel()
		resolved, err := dnsResolver.Resolve(rctx, ma)
		if err != nil || len(resolved) == 0 {
			log.Printf("p2p: DNS seed %q not resolvable yet, skipping: %v", addr, err)
			return nil
		}
		infos, err := peer.AddrInfosFromP2pAddrs(resolved...)
		if err != nil {
			log.Printf("p2p: DNS seed %q resolved to unusable addrs, skipping: %v", addr, err)
			return nil
		}
		return infos
	}
	ai, err := peer.AddrInfoFromString(addr)
	if err != nil {
		log.Printf("p2p: ignoring bad bootstrap addr %q: %v", addr, err)
		return nil
	}
	return []peer.AddrInfo{*ai}
}

// hasDnsaddr reports whether ma carries a /dnsaddr component, which needs TXT
// resolution to discover the peer ids behind it. /dns4 and /dns6 do NOT count: they
// carry an inline /p2p id and are resolved by the swarm at dial time.
func hasDnsaddr(ma multiaddr.Multiaddr) bool {
	_, err := ma.ValueForProtocol(multiaddr.P_DNSADDR)
	return err == nil
}
