//go:build libp2p

// libp2p adapter for the Network interface.
//
// Behind a build tag so the default build has zero networking dependencies and
// `go test ./...` runs the entire gossip protocol against InProc in
// milliseconds, with no ports, sleeps, or flakes.
//
// Enable:
//
//	go get github.com/libp2p/go-libp2p@latest
//	go get github.com/libp2p/go-libp2p-pubsub@latest
//	go build -tags libp2p ./...
//
// This file is a transport shim; the protocol logic (and its tests) lives
// above it. The conformance assertion at the bottom makes a signature drift a
// compile error under the tag.
package p2p

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	libp2pnet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/multiformats/go-multiaddr"
)

// Connection watermarks for the ConnectionManager. Sized for a small VPS: keep up
// to highWater peers, trim back toward lowWater when over, after a grace period so
// new peers get a chance. Bootstrap peers are Protect-ed from trimming. Without this
// libp2p uses a NullConnMgr — no trimming, no protection — so early junk connections
// can hold every slot and starve honest peers.
const (
	connLowWater  = 32
	connHighWater = 96
)

// banGater refuses a secured connection from a peer the shared Scorer has banned, so
// a ban binds to the connection, not just to dropped messages — a peer cannot keep a
// socket open to spend our resources after being banned. Per-IP caps are intentionally
// not added here: on a NAT many honest peers share an IP, so a naive cap would exclude
// them. The rest of the interface allows by default.
type banGater struct{ n *LibP2P }

func (g banGater) InterceptPeerDial(peer.ID) bool                      { return true }
func (g banGater) InterceptAddrDial(peer.ID, multiaddr.Multiaddr) bool { return true }
func (g banGater) InterceptAccept(libp2pnet.ConnMultiaddrs) bool       { return true }
func (g banGater) InterceptUpgraded(libp2pnet.Conn) (bool, control.DisconnectReason) {
	return true, 0
}
func (g banGater) InterceptSecured(_ libp2pnet.Direction, p peer.ID, _ libp2pnet.ConnMultiaddrs) bool {
	return !g.n.isBanned(p)
}

// DeterministicKey derives a stable libp2p identity from a seed string: same
// seed, same peer ID, on every machine and run, which lets a devnet write its
// bootstrap list ahead of time instead of discovering it.
//
// A real network uses a persisted random key; a predictable key is acceptable
// only for a devnet, whose threat model is just "does it connect on one
// laptop". The seed is hashed so any string works without accidental collision.
func DeterministicKey(seed string) (crypto.PrivKey, error) {
	h := sha256.Sum256([]byte(seed))
	priv, _, err := crypto.GenerateEd25519Key(bytes.NewReader(h[:]))
	return priv, err
}

// PeerIDForSeed returns the peer ID a node started with DeterministicKey(seed)
// will have. The devnet uses it to build each node's bootstrap multiaddrs
// without having to start the peers first and read them back.
func PeerIDForSeed(seed string) (string, error) {
	priv, err := DeterministicKey(seed)
	if err != nil {
		return "", err
	}
	id, err := peer.IDFromPublicKey(priv.GetPublic())
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// syncStreamTimeout bounds one request/response round-trip. A peer that opens a
// stream and stalls must not pin a syncing node forever; the deadline turns a
// stall into an ordinary failed request the caller retries elsewhere.
const syncStreamTimeout = 30 * time.Second

// maxRequestMessage caps a single request or response read off a stream, so a
// peer cannot stream unbounded data into memory. Mirrors maxSyncMessage in
// sync.go.
const maxRequestMessage = 16 << 20

// mdnsServiceTag scopes discovery to this network via the chain id. Without it,
// two devnets on the same LAN discover each other, fail the genesis check, and
// present as a peering bug.
func mdnsServiceTag(chainID uint64) string {
	return fmt.Sprintf("lxs-devnet-%d", chainID)
}

type LibP2P struct {
	host host.Host
	ps   *pubsub.PubSub
	ctx  context.Context
	stop context.CancelFunc

	kdht *dht.IpfsDHT // Kademlia DHT for decentralized peer discovery; nil until wired

	peersPath string // if set, where connected peers are snapshotted for restart-time re-dial

	// dialSem bounds concurrent discovery/warm-start dials. FindPeers can return up
	// to 100 attacker-chosen addresses every tick, and a restart can have dozens of
	// persisted hints; a goroutine-per-result dial would let a hostile result set burst
	// unbounded outbound dials. This caps the burst without serialising it.
	dialSem chan struct{}

	boots []peer.AddrInfo // parsed bootstrap peers, for an on-demand Redial

	// banCheck, if set, is consulted before (re)dialling a bootstrap peer. It
	// breaks the ban/re-dial oscillation: a banned peer is disconnected, and
	// without this the 2s re-dial loop would immediately reconnect it. Wired to
	// the shared Scorer's Banned after construction (the scorer does not exist
	// when keepConnected's goroutines start), so it is an atomic pointer read on
	// the dial path. nil means never skip.
	banCheck atomic.Pointer[func(PeerID) bool]

	mu     sync.Mutex
	topics map[Topic]*pubsub.Topic
	subs   []*pubsub.Subscription
}

// SetBanCheck wires the dialer's ban predicate after construction. Safe to call
// once the shared Scorer exists; the keepConnected goroutines pick it up on
// their next 2s tick.
func (n *LibP2P) SetBanCheck(f func(PeerID) bool) {
	n.banCheck.Store(&f)
}

// isBanned reports whether the dialer should currently refuse this bootstrap
// peer. Unset check means not banned, so an unwired node dials as before.
func (n *LibP2P) isBanned(id peer.ID) bool {
	if f := n.banCheck.Load(); f != nil {
		return (*f)(PeerID(id.String()))
	}
	return false
}

// appScore feeds the app Scorer's verdict into gossipsub. A banned peer scores far
// below every threshold so the mesh graylists it; an honest peer scores 0 (above all
// thresholds), so scoring never touches a well-behaved peer.
func (n *LibP2P) appScore(id peer.ID) float64 {
	if n.isBanned(id) {
		return -1000
	}
	return 0
}

type LibP2PConfig struct {
	// ListenPort is the TCP port. 0 picks a free one.
	ListenPort int
	// ChainID scopes mDNS discovery. Required.
	ChainID uint64
	// PrivKey is the network identity. Not the coinbase key and not any key that
	// signs consensus: its only job is to name a socket. It is exposed to every
	// peer by design and should be rotatable without touching funds; a key that
	// both signs blocks and terminates TCP connections has two threat models and
	// one copy. nil generates an ephemeral one.
	PrivKey crypto.PrivKey

	// Bootstrap is a list of peer multiaddrs to dial on startup, each of the form
	// /ip4/HOST/tcp/PORT/p2p/PEERID. Explicit peering that does not depend on
	// mDNS: on a devnet it is the difference between reliable convergence and a
	// node sitting alone because a multicast packet was dropped. mDNS stays on as
	// a bonus, not a requirement.
	//
	// Multiple comma-separated seeds are supported end-to-end: the -bootstrap flag
	// in cmd/lxs splits on "," (main.go), so each entry arrives here as its own
	// element and gets its own keepConnected goroutine. One seed is a single
	// discovery point; several remove that single point of failure.
	Bootstrap []string

	// PeersPath, if set, is a JSON file (in the node's datadir) where the currently
	// connected peers are snapshotted periodically and on shutdown, and re-loaded on
	// the next startup. Without it a restarted node whose every seed is down comes up
	// isolated; with it the node re-dials peers it already knew, so the network heals
	// across restarts instead of depending on a seed being reachable at that instant.
	// Empty disables persistence (missing/corrupt file is tolerated, never fatal).
	PeersPath string

	// DHTServerMode forces the Kademlia DHT into ModeServer instead of the production
	// default ModeAuto. Production wants ModeAuto: a public node becomes a DHT server,
	// a NAT'd home miner stays a client, which is correct for a real network. But
	// autonat cannot tell that a 127.0.0.1 node is publicly reachable, so on localhost
	// ModeAuto leaves every node a client and no routing table ever forms — which is
	// exactly what the DHT discovery test needs. Tests set this true to force server
	// mode; leave it false everywhere else.
	DHTServerMode bool

	// DisableMDNS turns off local-link (mDNS) discovery. Production leaves it ON (a
	// free bonus on a LAN). Tests that must prove DHT discovery in isolation set it
	// true: all test nodes run on 127.0.0.1, where mDNS would connect them regardless
	// of the DHT and turn a DHT test into a false green.
	DisableMDNS bool
}

// dhtRendezvous is the advertise/find key that scopes DHT peer discovery to this
// chain. Two chains sharing a bootstrap host would otherwise cross-discover, fail
// each other's genesis check, and look like a peering bug rather than a config one.
func dhtRendezvous(chainID uint64) string {
	return fmt.Sprintf("lxs/%d", chainID)
}

// dhtDiscoveryInterval is how often the discovery loop re-queries the DHT for peers
// advertising our rendezvous. Frequent enough that a node joining an established
// network converges in seconds, not so frequent it hammers the routing table.
const dhtDiscoveryInterval = 15 * time.Second

// peerSnapshotInterval is how often the connected peer set is written to PeersPath.
// A crash between snapshots loses at most this much of the peer memory, which only
// costs a slower reconnect on the next boot, never correctness.
const peerSnapshotInterval = 60 * time.Second

// maxPersistedPeers bounds the peer snapshot file AND the warm-start dials it drives.
// A restarted node needs only a handful of reachable peers to re-enter the DHT, which
// re-discovers the rest; a large file just turns startup into a dial storm and gives a
// poisoned file more attacker-chosen dials. Small on purpose.
const maxPersistedPeers = 64

// maxConcurrentDials caps in-flight discovery/warm-start dials (dialSem depth), so a
// hostile FindPeers result set or a poisoned peers file cannot burst unbounded outbound
// connections. The connmgr and resource manager bound the resting state; this bounds
// the transient.
const maxConcurrentDials = 16

// IP-group diversity caps for the DHT routing table: at most this many peers from one
// IP group (roughly a /24 v4 or /48 v6) per bucket and across the whole table. Without
// a diversity filter a single-subnet sybil fleet can fill the routing table and eclipse
// the node; kad-dht ships this filter, we just have to enable it. Values follow the
// conservative end of what IPFS uses.
const (
	maxPeersPerIPGroupCpl   = 2
	maxPeersPerIPGroupTable = 3
)

// maxPeersFileBytes caps how much of peers.json is read at startup, so a crafted or
// corrupt file (write access to the datadir) cannot OOM the node before it parses.
const maxPeersFileBytes = 1 << 20 // 1 MiB — far above 64 multiaddrs

func NewLibP2P(ctx context.Context, cfg LibP2PConfig) (*LibP2P, error) {
	if cfg.ChainID == 0 {
		return nil, fmt.Errorf("p2p: ChainID is required — mDNS without it discovers other people's devnets")
	}

	priv := cfg.PrivKey
	if priv == nil {
		var err error
		priv, _, err = crypto.GenerateKeyPair(crypto.Ed25519, -1)
		if err != nil {
			return nil, err
		}
	}

	listen, err := multiaddr.NewMultiaddr(
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", cfg.ListenPort))
	if err != nil {
		return nil, err
	}

	// n is created before the host so the ban gater can reference it; the gater reads
	// n.isBanned, which is nil-safe until SetBanCheck wires the scorer.
	n := &LibP2P{topics: make(map[Topic]*pubsub.Topic), dialSem: make(chan struct{}, maxConcurrentDials)}

	cm, err := connmgr.NewConnManager(connLowWater, connHighWater)
	if err != nil {
		return nil, err
	}
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrs(listen),
		libp2p.ConnectionManager(cm),        // bound + trim connections; protect bootstrap peers below
		libp2p.ConnectionGater(banGater{n}), // refuse a secured connection from a banned peer
	)
	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithCancel(ctx)

	// GossipSub, not FloodSub. FloodSub sends every message to every peer:
	// quadratic at scale. GossipSub maintains a mesh and gossips metadata, which
	// is also why it delivers duplicates, hence HasBlock first in onBlock.
	//
	// Peer scoring layers the app Scorer into the mesh: AppSpecificScore drives a
	// banned peer's score deep below every threshold so gossipsub graylists/prunes it
	// at the mesh level too, not just after decode. Deliberately CONSERVATIVE — only
	// app-banned peers score below zero; an honest peer scores exactly 0 and is above
	// every threshold, so this can never prune a well-behaved peer. All native
	// gossipsub penalty params are left off so nothing else can misfire.
	scoreParams := &pubsub.PeerScoreParams{
		AppSpecificScore:  func(p peer.ID) float64 { return n.appScore(p) },
		AppSpecificWeight: 1,
		DecayInterval:     time.Minute,
		DecayToZero:       0.01,
		Topics:            make(map[string]*pubsub.TopicScoreParams),
	}
	scoreThresholds := &pubsub.PeerScoreThresholds{
		GossipThreshold:   -100,
		PublishThreshold:  -200,
		GraylistThreshold: -500,
	}
	ps, err := pubsub.NewGossipSub(cctx, h, pubsub.WithPeerScore(scoreParams, scoreThresholds))
	if err != nil {
		cancel()
		h.Close()
		return nil, err
	}

	n.host = h
	n.ps = ps
	n.ctx = cctx
	n.stop = cancel

	// mDNS: peers announce themselves on the local link. Zero config, but
	// intermittently unreliable on this stack — a node can hold TCP connections
	// that never finish the libp2p handshake, leaving it meshed with nobody. So
	// mDNS is a bonus, not the plan.
	if !cfg.DisableMDNS {
		svc := mdns.NewMdnsService(h, mdnsServiceTag(cfg.ChainID), &mdnsNotifee{h: h, ctx: cctx})
		if err := svc.Start(); err != nil {
			cancel()
			h.Close()
			return nil, err
		}
	}

	n.peersPath = cfg.PeersPath
	var bootInfos []peer.AddrInfo // parsed entry points handed to the DHT below

	// Operator-configured bootstrap peers are TRUSTED: dial them directly, keep them
	// connected for the node's lifetime, protect them from connmgr trimming, and use
	// them as DHT entry points. Independent of whether mDNS found anyone.
	for _, addr := range cfg.Bootstrap {
		ai, err := peer.AddrInfoFromString(addr)
		if err != nil {
			log.Printf("p2p: ignoring bad bootstrap addr %q: %v", addr, err)
			continue
		}
		if ai.ID == h.ID() {
			continue // ourselves
		}
		bootInfos = append(bootInfos, *ai)
		n.boots = append(n.boots, *ai)
		h.ConnManager().Protect(ai.ID, "bootstrap") // never trim an operator-chosen seed
		go n.keepConnected(*ai)
	}

	// Peers remembered from a prior run are a WARM-START HINT, NOT trusted. They were
	// merely observed on the network last time, so they are attacker-influenceable: a
	// sybil that got into the live peer set once must NOT be promoted, across a reboot,
	// to an un-trimmable, permanently re-dialed, protected seed — that would be an
	// eclipse that survives restarts, and it would also let the persisted set (up to
	// maxPersistedPeers) blow past the connmgr high-water and starve honest peers. So
	// each is dialed ONCE (through dialSem) as a DHT entry point and then treated like
	// any other peer: never Protect-ed, never keepConnected. The DHT and honest peers
	// take over; a stale or hostile entry simply fails to connect and ages out.
	for _, addr := range loadPersistedPeers(cfg.PeersPath) {
		ai, err := peer.AddrInfoFromString(addr)
		if err != nil || ai.ID == h.ID() {
			continue
		}
		bootInfos = append(bootInfos, *ai)
		hint := *ai
		go func() {
			n.dialSem <- struct{}{}
			defer func() { <-n.dialSem }()
			_ = h.Connect(cctx, hint)
		}()
	}

	// Kademlia DHT: decentralized peer discovery so the network survives at any
	// scale without depending on a single seed. Bootstrap peers are only the way IN;
	// once a node is in the DHT it learns of peers the seed never told it about, so a
	// seed going down after a node has joined does not isolate it — the exact
	// single-point-of-failure the hardcoded bootstrap list has on its own.
	//
	// ProtocolPrefix scopes the DHT to this chain (/lxs/<chainID>) so LXS nodes only
	// ever populate their routing table with other LXS nodes, never the public IPFS
	// DHT. Without the prefix a node would answer, and be answered by, every IPFS node
	// on the internet — a privacy and correctness leak, and a genesis-mismatch storm.
	dhtMode := dht.ModeAuto
	if cfg.DHTServerMode {
		dhtMode = dht.ModeServer
	}
	kdht, err := dht.New(h,
		dht.ProtocolPrefix(protocol.ID(fmt.Sprintf("/lxs/%d", cfg.ChainID))),
		dht.Mode(dhtMode),
		dht.BootstrapPeers(bootInfos...),
		// Eclipse defence: cap how many peers from one IP group (~/24 v4, /48 v6) may
		// occupy the routing table, so a single-subnet sybil fleet cannot fill it and
		// isolate the node from honest peers. kad-dht ships this filter; we enable it.
		dht.RoutingTablePeerDiversityFilter(dht.NewRTPeerDiversityFilter(h, maxPeersPerIPGroupCpl, maxPeersPerIPGroupTable)),
	)
	if err != nil {
		cancel()
		h.Close()
		return nil, fmt.Errorf("p2p: creating DHT: %w", err)
	}
	if err := kdht.Bootstrap(cctx); err != nil {
		_ = kdht.Close()
		cancel()
		h.Close()
		return nil, fmt.Errorf("p2p: bootstrapping DHT: %w", err)
	}
	n.kdht = kdht
	go n.discoverLoop(cfg.ChainID)

	// Peer persistence: snapshot the live peer set to disk on a timer and on
	// shutdown, so the next boot has a warm start even if no seed answers.
	if cfg.PeersPath != "" {
		go n.persistLoop()
	}

	return n, nil
}

// discoverLoop advertises this node under the chain's rendezvous and repeatedly
// asks the DHT for other nodes advertising the same key, dialling any it is not
// already connected to. This is what turns "I know one seed" into "I know the
// network": FindPeers returns peers the seed never directly told us about. Banned
// peers are skipped so discovery cannot undo a warden's disconnect. Stops on ctx.
func (n *LibP2P) discoverLoop(chainID uint64) {
	routingDisc := drouting.NewRoutingDiscovery(n.kdht)
	rendezvous := dhtRendezvous(chainID)

	// Advertise once immediately, then let the util keep the advertisement fresh; it
	// re-provides on its own schedule so a node stays findable for its whole lifetime.
	dutil.Advertise(n.ctx, routingDisc, rendezvous)

	// One immediate pass so a freshly-started node does not wait a full interval before
	// its first discovery; then on the ticker.
	find := func() {
		fctx, cancel := context.WithTimeout(n.ctx, dhtDiscoveryInterval)
		defer cancel()
		peers, err := routingDisc.FindPeers(fctx, rendezvous)
		if err != nil {
			return // transient: an empty or not-yet-warm routing table, retried next tick
		}
		for pi := range peers {
			if pi.ID == n.host.ID() || len(pi.Addrs) == 0 {
				continue
			}
			if n.isBanned(pi.ID) {
				continue // do not re-dial a peer the warden banned
			}
			if n.host.Network().Connectedness(pi.ID) == libp2pnet.Connected {
				continue // already meshed with this peer
			}
			pi := pi
			go func() {
				n.dialSem <- struct{}{}
				defer func() { <-n.dialSem }()
				_ = n.host.Connect(n.ctx, pi)
			}()
		}
	}
	find()

	ticker := time.NewTicker(dhtDiscoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			find()
		}
	}
}

// Redial forces an immediate reconnect attempt to every bootstrap peer we are
// not currently connected to. keepConnected already re-dials on a 2s loop; this
// skips the wait when isolation is detected. Best-effort and non-blocking (each
// dial runs in its own goroutine).
func (n *LibP2P) Redial() {
	for _, ai := range n.boots {
		if n.host.Network().Connectedness(ai.ID) == libp2pnet.Connected {
			continue
		}
		if n.isBanned(ai.ID) {
			continue // this peer was banned and cut; do not undo that
		}
		ai := ai
		go func() { _ = n.host.Connect(n.ctx, ai) }()
	}
}

// keepConnected dials a bootstrap peer and re-dials whenever the connection is
// lost, for the node's lifetime. Unlike mDNS's fire-and-forget dial, it logs
// failures, the only way the "connected at TCP, never at libp2p" flakiness is
// seen instead of swallowed.
func (n *LibP2P) keepConnected(ai peer.AddrInfo) {
	for attempt := 0; ; attempt++ {
		if n.ctx.Err() != nil {
			return
		}
		// A banned bootstrap peer is left disconnected: re-dialling it every 2s
		// is the oscillation isBanned prevents. Once the Scorer's penalty decays
		// below the threshold, isBanned goes false and dialling resumes: a
		// cooldown, not a permanent cut.
		if !n.isBanned(ai.ID) && n.host.Network().Connectedness(ai.ID) != libp2pnet.Connected {
			if err := n.host.Connect(n.ctx, ai); err != nil {
				if attempt%10 == 0 { // throttle: a peer not yet up is normal
					log.Printf("p2p: bootstrap dial %s failed: %v", ai.ID, err)
				}
			}
		}
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// mdnsNotifee dials peers as mDNS finds them.
type mdnsNotifee struct {
	h   host.Host
	ctx context.Context
}

func (m *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == m.h.ID() {
		return // ourselves
	}
	// Errors ignored on purpose: a peer that appears and cannot be dialled is
	// normal (shutting down, on another interface, or already connected). mDNS
	// fires repeatedly, so a failed dial gets another chance.
	_ = m.h.Connect(m.ctx, pi)
}

func (n *LibP2P) Self() PeerID { return PeerID(n.host.ID().String()) }

func (n *LibP2P) topic(t Topic) (*pubsub.Topic, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if existing, ok := n.topics[t]; ok {
		return existing, nil
	}
	// ps.Join errors if called twice for a topic, hence the cache.
	joined, err := n.ps.Join(string(t))
	if err != nil {
		return nil, err
	}
	n.topics[t] = joined
	return joined, nil
}

func (n *LibP2P) Publish(t Topic, data []byte) error {
	tp, err := n.topic(t)
	if err != nil {
		return err
	}
	return tp.Publish(n.ctx, data)
}

func (n *LibP2P) Subscribe(t Topic, h Handler) error {
	tp, err := n.topic(t)
	if err != nil {
		return err
	}
	sub, err := tp.Subscribe()
	if err != nil {
		return err
	}

	n.mu.Lock()
	n.subs = append(n.subs, sub)
	n.mu.Unlock()

	go func() {
		for {
			msg, err := sub.Next(n.ctx)
			if err != nil {
				return // context cancelled or subscription closed
			}
			// Skip our own messages: GossipSub echoes publishes back. Without
			// this, every node processes its own blocks as if a stranger sent
			// them. HasBlock catches it, but it makes the duplicate counters lie.
			if msg.ReceivedFrom == n.host.ID() {
				continue
			}
			// One goroutine per topic, sequential. Handlers touch the chain,
			// which has its own lock; fanning out here buys only contention and
			// non-determinism. A handler error scores msg.ReceivedFrom. Behind a
			// panic firewall: a hostile message must not crash this goroutine.
			_ = safeHandle(h, PeerID(msg.ReceivedFrom.String()), msg.Data)
		}
	}()
	return nil
}

// SetRequestHandler registers a stream handler for a request/response
// protocol. One stream = one request in, one response out, then closed.
func (n *LibP2P) SetRequestHandler(proto Protocol, h RequestHandler) error {
	n.host.SetStreamHandler(protocol.ID(proto), func(s libp2pnet.Stream) {
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(syncStreamTimeout))
		from := PeerID(s.Conn().RemotePeer().String())

		req, err := readCapped(s, maxRequestMessage)
		if err != nil {
			_ = s.Reset()
			return
		}
		resp, err := safeRequest(h, from, req)
		if err != nil {
			// Reset, not a graceful close: a reset tells the caller the request
			// failed rather than handing it an empty body it might read as a
			// valid "nothing to send".
			_ = s.Reset()
			return
		}
		_, _ = s.Write(resp) // flushed by the deferred Close
	})
	return nil
}

// Request opens a stream to one peer, writes req, half-closes, and reads the
// response. The write half is closed so the server sees EOF and knows the
// request is complete without a length prefix.
func (n *LibP2P) Request(to PeerID, proto Protocol, req []byte) ([]byte, error) {
	pid, err := peer.Decode(string(to))
	if err != nil {
		return nil, fmt.Errorf("p2p: undecodable peer %q: %w", to, err)
	}

	ctx, cancel := context.WithTimeout(n.ctx, syncStreamTimeout)
	defer cancel()

	s, err := n.host.NewStream(ctx, pid, protocol.ID(proto))
	if err != nil {
		return nil, err
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(syncStreamTimeout))

	if _, err := s.Write(req); err != nil {
		_ = s.Reset()
		return nil, err
	}
	if err := s.CloseWrite(); err != nil {
		_ = s.Reset()
		return nil, err
	}
	resp, err := readCapped(s, maxRequestMessage)
	if err != nil {
		_ = s.Reset()
		return nil, err
	}
	return resp, nil
}

// readCapped reads all of r but refuses more than max bytes, so a peer cannot
// stream unbounded data into memory. It reads max+1 and checks the length:
// io.LimitReader alone truncates silently, turning an oversized message into a
// corrupt short one.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("p2p: message exceeds %d bytes", max)
	}
	return data, nil
}

func (n *LibP2P) Peers() []PeerID {
	ps := n.host.Network().Peers()
	out := make([]PeerID, 0, len(ps))
	for _, p := range ps {
		out = append(out, PeerID(p.String()))
	}
	return out
}

// Disconnect closes every connection to a peer, identified by the same string
// form Peers returns. Used to enforce a Scorer ban at the connection.
func (n *LibP2P) Disconnect(id string) error {
	pid, err := peer.Decode(id)
	if err != nil {
		return fmt.Errorf("p2p: bad peer id %q: %w", id, err)
	}
	return n.host.Network().ClosePeer(pid)
}

func (n *LibP2P) Close() error {
	// Snapshot before tearing down so the last-known good peer set survives a clean
	// shutdown; a node restarted right after this comes up with a warm peer memory
	// even if every seed is unreachable at that moment.
	if n.peersPath != "" {
		n.snapshotPeers()
	}
	n.stop()
	if n.kdht != nil {
		_ = n.kdht.Close()
	}
	n.mu.Lock()
	for _, s := range n.subs {
		s.Cancel()
	}
	for _, t := range n.topics {
		_ = t.Close()
	}
	n.mu.Unlock()
	return n.host.Close()
}

// persistLoop snapshots the connected peer set to PeersPath on a timer for the
// node's lifetime. The on-shutdown snapshot lives in Close; this is the periodic
// floor under it, so a node that crashes still has a recent-enough peer file. Stops
// on ctx.
func (n *LibP2P) persistLoop() {
	ticker := time.NewTicker(peerSnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			n.snapshotPeers()
		}
	}
}

// snapshotPeers writes the currently-connected peers' dialable multiaddrs to
// PeersPath as JSON, bounded to maxPersistedPeers. Best-effort: a write failure is
// logged, not fatal — losing the snapshot only costs a colder next boot, never
// correctness. The write is atomic (temp file + rename) so a crash mid-write cannot
// leave a truncated file that the next boot fails to parse.
func (n *LibP2P) snapshotPeers() {
	if n.peersPath == "" {
		return
	}
	var addrs []string
	for _, pid := range n.host.Network().Peers() {
		if len(addrs) >= maxPersistedPeers {
			break
		}
		ai := n.host.Peerstore().PeerInfo(pid)
		if len(ai.Addrs) == 0 {
			continue
		}
		s, err := peer.AddrInfoToP2pAddrs(&ai)
		if err != nil {
			continue
		}
		for _, a := range s {
			if len(addrs) >= maxPersistedPeers {
				break // bound the TOTAL: a multi-address peer must not overflow the cap
			}
			addrs = append(addrs, a.String())
		}
	}
	data, err := json.Marshal(addrs)
	if err != nil {
		return
	}
	tmp := n.peersPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("p2p: writing peer snapshot %q: %v", n.peersPath, err)
		return
	}
	if err := os.Rename(tmp, n.peersPath); err != nil {
		log.Printf("p2p: renaming peer snapshot %q: %v", n.peersPath, err)
	}
}

// loadPersistedPeers reads a peer snapshot written by a prior run and returns its
// multiaddrs, to be dialled as extra bootstrap entries. A missing file is the normal
// first-run case and returns nothing; a corrupt file is logged and ignored rather
// than crashing the node — a bad cache must never stop a node from starting, since
// it can always re-discover peers through the seed and the DHT.
func loadPersistedPeers(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("p2p: reading peer snapshot %q: %v", path, err)
		}
		return nil
	}
	defer f.Close()
	// Bound the read: a crafted or corrupt peers.json (write access to the datadir)
	// must not OOM the node at startup by claiming to be gigabytes.
	data, err := io.ReadAll(io.LimitReader(f, maxPeersFileBytes))
	if err != nil {
		log.Printf("p2p: reading peer snapshot %q: %v", path, err)
		return nil
	}
	var addrs []string
	if err := json.Unmarshal(data, &addrs); err != nil {
		log.Printf("p2p: ignoring corrupt peer snapshot %q: %v", filepath.Clean(path), err)
		return nil
	}
	if len(addrs) > maxPersistedPeers {
		addrs = addrs[:maxPersistedPeers]
	}
	return addrs
}

// Addrs returns this host's dialable addresses. For logging: the host's own
// multiaddr is the quickest confirmation it came up.
func (n *LibP2P) Addrs() []string {
	out := []string{}
	for _, a := range n.host.Addrs() {
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, n.host.ID()))
	}
	return out
}

// Compile-time proof the adapter satisfies the interface: a signature drift is
// a build error under the tag, not a runtime surprise.
var _ Network = (*LibP2P)(nil)
