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
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	libp2pnet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
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
	Bootstrap []string
}

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
	n := &LibP2P{topics: make(map[Topic]*pubsub.Topic)}

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
	svc := mdns.NewMdnsService(h, mdnsServiceTag(cfg.ChainID), &mdnsNotifee{h: h, ctx: cctx})
	if err := svc.Start(); err != nil {
		cancel()
		h.Close()
		return nil, err
	}

	// Explicit bootstrap peers: dial known addresses directly and keep them
	// connected, independent of whether mDNS found anyone.
	for _, addr := range cfg.Bootstrap {
		ai, err := peer.AddrInfoFromString(addr)
		if err != nil {
			log.Printf("p2p: ignoring bad bootstrap addr %q: %v", addr, err)
			continue
		}
		if ai.ID == h.ID() {
			continue // ourselves
		}
		n.boots = append(n.boots, *ai)
		h.ConnManager().Protect(ai.ID, "bootstrap") // never trim the seed under connection pressure
		go n.keepConnected(*ai)
	}

	return n, nil
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
	n.stop()
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
