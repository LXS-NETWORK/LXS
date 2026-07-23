//go:build libp2p

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"path/filepath"
	"time"

	"lxs/core"
	"lxs/health"
	"lxs/mempool"
	"lxs/p2p"
	"lxs/types"
)

// p2pID prints the peer ID a node started with -p2p-seed <seed> will have.
// Deterministic identities let the devnet write each node's bootstrap list
// before any node starts.
func p2pID(args []string) error {
	fs := flag.NewFlagSet("p2p-id", flag.ExitOnError)
	seed := fs.String("seed", "", "identity seed")
	fs.Parse(args)
	id, err := p2p.PeerIDForSeed(*seed)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

// syncInterval is how often a node proactively syncs from a peer.
//
// Gap-triggered sync (an orphan off gossip) only fires for a node that receives a
// block it cannot place; a node starved of blocks by its mesh position never
// triggers. The periodic tick is the floor under that, so convergence does not
// depend on a kind mesh. SyncFrom dedupes, so a tick during an in-flight sync is free.
const syncInterval = 3 * time.Second

// startP2P brings up libp2p block gossip and wires the producer's announce hook.
//
// Behind the libp2p build tag, reached only through the identical signature in
// p2p_stub.go. main.go never names a libp2p symbol, so the default build stays
// dependency-free.
//
// Receive and send are wired separately on purpose:
//   - receive: NewGossip subscribes to the blocks topic and inserts inbound
//     blocks through the same validation path as everything else. A block off
//     the wire earns no trust for arriving.
//   - send: SetOnBlock fires AFTER a block is in the local chain. Announcing
//     first would advertise a block that might then fail to insert. On a
//     follower the hook is set but never fires — followers do not seal.
//
// p2pHandles bundles what the node keeps from a started stack: Close, the tx
// broadcaster, and the health seams (Peers, Resync, Redial). A struct keeps
// startP2P's signature stable as seams are added.
type p2pHandles struct {
	Close      func() error
	Broadcast  func(*types.Transaction) error
	Peers      func() []health.PeerHealth
	Resync     func(context.Context) error
	Redial     func(context.Context) error
	Disconnect func(id string) error // enforce a ban at the connection
}

func startP2P(ctx context.Context, bc *core.Blockchain, pool *mempool.Mempool, prod *core.Producer, port int, seed string, bootstrap []string, datadir string) (*p2pHandles, error) {
	cfg := p2p.LibP2PConfig{
		ListenPort: port,
		ChainID:    bc.ChainID(),
		Bootstrap:  bootstrap,
	}
	// Persist the peer set only when there is a durable datadir to hold it. With an
	// in-memory node there is nowhere to write, and a peers file next to a chain that
	// vanishes on restart would point at a network the node can no longer verify.
	if datadir != "" {
		cfg.PeersPath = filepath.Join(datadir, "peers.json")
	}
	// A stable identity so bootstrap peers can name it in advance. Empty seed
	// means an ephemeral key and mDNS-only discovery.
	if seed != "" {
		priv, err := p2p.DeterministicKey(seed)
		if err != nil {
			return nil, fmt.Errorf("p2p: deriving identity from seed: %w", err)
		}
		cfg.PrivKey = priv
	}

	net, err := p2p.NewLibP2P(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("p2p: starting libp2p: %w", err)
	}

	// One scorer shared across all three protocols: a peer misbehaving on blocks,
	// txs, or sync accumulates penalties in one tally, and a ban earned anywhere
	// applies everywhere.
	scorer := p2p.NewScorer(0) // default threshold

	// Let the bootstrap dialer skip banned peers so it stops fighting the warden
	// that disconnects them; dialling resumes when a ban decays. Wired here because
	// the scorer does not exist until now.
	net.SetBanCheck(scorer.Banned)

	// Syncer before gossip: gossip's gap signal drives it, so it must exist first.
	// NewSyncer also registers the server side, so the node can answer catch-up
	// requests from the moment it is up.
	syncer, err := p2p.NewSyncer(net, bc,
		p2p.WithSyncLogger(log.Default()),
		p2p.WithSyncScorer(scorer),
	)
	if err != nil {
		_ = net.Close()
		return nil, fmt.Errorf("p2p: starting syncer: %w", err)
	}

	// The peers probe: for each connected peer, its score and ban status from the
	// shared Scorer. Recomputed each call so a snapshot reflects the live peer set.
	// Defined here (before gossip) because the gap handler needs it to pick a
	// best-scored sync target.
	peers := func() []health.PeerHealth {
		ids := net.Peers()
		out := make([]health.PeerHealth, 0, len(ids))
		for _, id := range ids {
			out = append(out, health.PeerHealth{ID: string(id), Penalty: scorer.Penalty(id), Banned: scorer.Banned(id)})
		}
		return out
	}

	// go, not a direct call: SyncFrom blocks on network round-trips and must not
	// stall the gossip goroutine that delivered the orphan. The target is NOT
	// blindly `from`: PickGapSyncPeer steers to a best-scored peer so a flaky
	// (penalised-but-unbanned) announcer cannot pin our catch-up to itself.
	g, err := p2p.NewGossip(net, bc,
		p2p.WithLogger(log.Default()),
		p2p.WithScorer(scorer),
		p2p.WithGapHandler(func(from p2p.PeerID) {
			if target, ok := health.PickGapSyncPeer(string(from), peers()); ok {
				go syncer.SyncFrom(p2p.PeerID(target))
			}
		}),
	)
	if err != nil {
		_ = net.Close()
		return nil, fmt.Errorf("p2p: starting gossip: %w", err)
	}

	// Tx gossip: propagates transactions and guards the mempool that receives
	// them. Same net, its own topic, its own validation, the same scorer.
	txg, err := p2p.NewTxGossip(net, bc, pool,
		p2p.WithTxLogger(log.Default()),
		p2p.WithTxScorer(scorer),
	)
	if err != nil {
		_ = net.Close()
		return nil, fmt.Errorf("p2p: starting tx gossip: %w", err)
	}

	prod.SetOnBlock(func(b *types.Block) {
		if err := g.Announce(b); err != nil {
			log.Printf("p2p: announce block %s failed: %v", b.Hash().Hex(), err)
		}
	})

	// Re-announce pending txs on a timer. GossipSub only delivers a tx to peers
	// SUBSCRIBED when it is first published; a miner that joins later (e.g. a user
	// opening the miner app) never receives a tx that was already waiting, so it
	// mines empty blocks while that tx starves — observed live, and exactly the
	// failure mode of casual miners that come and go. Re-publishing the mempool
	// every 15s guarantees any miner gets every pending tx within one interval of
	// connecting. GossipSub dedups and onTx treats ErrAlreadyKnown as a no-op, so
	// peers that already hold a tx drop the repeat cheaply. Bounded to one block's
	// worth of txs (mempool.Pending fills to the gas limit) so it can't flood.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				head := bc.Head()
				if head == nil {
					continue
				}
				for _, tx := range pool.Pending(bc.StateSnapshot(), head.Header.GasLimit) {
					_ = txg.Broadcast(tx)
				}
			}
		}
	}()

	for _, a := range net.Addrs() {
		log.Printf("p2p listening: %s", a)
	}

	// periodicSync self-tunes: it prefers the best-scored peers and adapts its
	// cadence, using the same peer telemetry the monitor and warden read.
	go periodicSync(ctx, net, syncer, bc, peers)
	// Healing seams. Resync: catch up from a random peer when the head stalls.
	// Redial: force an immediate reconnect to bootstrap peers when isolated. Both
	// best-effort — the healer throttles them.
	resync := func(context.Context) error {
		ids := net.Peers()
		if len(ids) == 0 {
			return fmt.Errorf("no peers to resync from")
		}
		go syncer.SyncFrom(ids[rand.Intn(len(ids))])
		return nil
	}
	redial := func(context.Context) error {
		net.Redial()
		return nil
	}
	return &p2pHandles{
		Close: net.Close, Broadcast: txg.Broadcast, Peers: peers,
		Resync: resync, Redial: redial, Disconnect: net.Disconnect,
	}, nil
}

// periodicSync catches up on a self-tuning timer. Two adaptations:
//
//   - Peer selection: syncs from the best-scored peers (RankSyncPeers — never
//     banned, lowest penalty first, random among the leading tier), steering
//     traffic away from flaky peers.
//   - Cadence: an AdaptiveBackoff tightens toward Min while the head advances and
//     relaxes toward Max when it does not. The advancing signal is head height,
//     which also moves from local mining or gossip, so on an active chain the node
//     sits near Min and the relax-to-Max saving only shows on a quiet chain.
//
// Stops when ctx is cancelled.
func periodicSync(ctx context.Context, net *p2p.LibP2P, syncer *p2p.Syncer, bc *core.Blockchain, peers func() []health.PeerHealth) {
	back := &health.AdaptiveBackoff{Min: syncInterval, Max: 10 * syncInterval, Grow: 1.5, Shrink: 0.5}
	lastHeight := bc.Head().Height()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(back.Value()):
		}
		ranked := health.RankSyncPeers(peers())
		if len(ranked) == 0 {
			back.Relax() // nobody good to sync from — ease off
			continue
		}
		// Random pick among the equally-best peers: prefer good peers without hammering one.
		leaders := health.LeadingTier(ranked)
		go syncer.SyncFrom(p2p.PeerID(leaders[rand.Intn(len(leaders))]))

		if h := bc.Head().Height(); h > lastHeight {
			back.Tighten() // advanced — more may be coming, sync harder
			lastHeight = h
		} else {
			back.Relax() // caught up — spare the network
		}
	}
}
