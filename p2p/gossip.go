package p2p

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"lxs/common"
	"lxs/core"
	"lxs/types"
)

var (
	ErrMessageTooLarge = errors.New("p2p: message too large")
	ErrBadEncoding     = errors.New("p2p: malformed message")
	ErrForgedHash      = errors.New("p2p: block does not hash to its claimed identity")
)

// maxBlockMessage caps an inbound block message. json.Unmarshal on an unbounded
// slice is a peer's cheapest way to exhaust memory.
const maxBlockMessage = 8 << 20 // 8 MiB — well above a full 30M-gas block

// DefaultMaxOrphans bounds the orphan queue. "Unknown parent" is normal (gossip
// has no ordering, so a child routinely beats its parent), so the queue is
// legitimate, but it also lets a peer send blocks with random parent hashes that
// never resolve. Unbounded, that exhausts memory. Bounded, a junk burst at worst
// evicts some honest orphans, which gossip re-delivers.
const DefaultMaxOrphans = 256

// DefaultMaxOrphanBytes caps the orphan queue's total memory. 256 orphans × a
// multi-MiB block is hundreds of MB — an OOM on the ~1 GiB VPS this targets. 32 MiB
// is ample for legitimate one-hop-ahead orphans while making the queue a non-threat.
const DefaultMaxOrphanBytes = 32 << 20 // 32 MiB

// blockMessage is the wire format. JSON, like storage, and safe for the same
// reason: the receiver re-derives the hash and refuses any mismatch. The
// encoding is trusted only to round-trip, then checked.
type blockMessage struct {
	Block *types.Block `json:"block"`
}

// Gossip is the block propagation protocol. It does not request anything: no
// request/response, no height negotiation (that is sync). It answers only
// whether an announced block reaches everyone and everyone ends up agreeing.
type Gossip struct {
	net Network
	bc  *core.Blockchain
	log *log.Logger

	// onGap fires when a block arrives whose parent we lack: the one signal
	// gossip has that we may be behind. Sync hangs off this hook so core stays
	// unaware of it, as it stays unaware of p2p. nil on a node with no syncer.
	onGap func(from PeerID)

	// scorer bans peers that keep misbehaving. nil disables scoring.
	scorer *Scorer

	maxOrphans int
	// maxOrphansPerPeer bounds how much of the queue one peer may hold. The
	// global bound alone is not enough: one peer flooding valid-looking blocks
	// with unknown parents (not rejectable) could evict every other peer's
	// orphans. Per-peer bounding limits it to its own quota.
	maxOrphansPerPeer int
	// maxOrphanBytes bounds the TOTAL memory the orphan queue may hold. The count
	// bound alone is not enough: an orphan's real difficulty cannot be checked until
	// its parent arrives (LWMA needs the ancestry), so a peer with no hashpower can
	// park blocks that merely CLAIM a low difficulty — each up to a full block of
	// attacker-chosen calldata. maxOrphans×maxBlockMessage is hundreds of MB, an OOM
	// on the small VPS this is meant to run on. Bounding cumulative bytes caps the
	// memory absolutely, regardless of count, per-peer share, or Sybil identities.
	maxOrphanBytes int

	mu sync.Mutex
	// orphans holds blocks whose parent has not arrived, keyed by that parent.
	// Each entry records its sender so the per-peer quota can be enforced and
	// refunded on eviction/resolution.
	orphans map[common.Hash][]orphanEntry
	// orphansByPeer is each peer's current share of the queue.
	orphansByPeer map[PeerID]int
	// orphanOrder is insertion order, for FIFO eviction. Oldest first: the block
	// that has waited longest is least likely to ever resolve.
	orphanOrder []common.Hash
	orphanCount int
	// orphanBytes is the running sum of parked orphans' approximate sizes, held
	// under maxOrphanBytes.
	orphanBytes int

	Stats Stats
}

// orphanEntry is a parked block plus its sender, so its resources can be charged
// to and refunded from that peer. size is the block's approximate memory footprint,
// recorded at queue time so eviction/resolution can subtract exactly what was added.
type orphanEntry struct {
	blk  *types.Block
	from PeerID
	size int
}

// orphanBytesOf approximates a block's parked memory: a small fixed header/overhead
// plus each tx's fixed fields and its calldata — the calldata is the bulk and the
// only part an attacker can inflate toward the message-size cap.
func orphanBytesOf(blk *types.Block) int {
	n := 256 // header + slice/map overhead
	for _, tx := range blk.Txs {
		n += 160 + len(tx.Data)
	}
	return n
}

type Stats struct {
	Received int

	// FastDuplicate: caught by HasBlock before decoding. LateDuplicate: caught
	// by InsertBlock returning ErrKnownBlock, meaning the cheap check missed and
	// a lookup plus validation entry was spent. Kept separate because on a
	// healthy mesh nearly every duplicate must be fast; a rising LateDuplicate
	// signals the cheap path is broken.
	FastDuplicate int
	LateDuplicate int

	Accepted int
	Orphaned int
	Evicted  int
	Rejected int
	// Dropped: messages ignored because the sender is banned. Counted apart from
	// Rejected, which is what gets a peer banned.
	Dropped int
	// Deferred: blocks refused for being too far in the future. Not misbehaviour,
	// so counted apart from Rejected and never penalised.
	Deferred int
}

type Option func(*Gossip)

func WithMaxOrphans(n int) Option { return func(g *Gossip) { g.maxOrphans = n } }

// WithMaxOrphansPerPeer bounds how much of the orphan queue one peer may hold.
// Defaults to a quarter of the total. Exposed mainly for tests.
func WithMaxOrphansPerPeer(n int) Option { return func(g *Gossip) { g.maxOrphansPerPeer = n } }

// WithMaxOrphanBytes bounds the total memory the orphan queue may hold. Defaults to
// DefaultMaxOrphanBytes. Exposed mainly for tests.
func WithMaxOrphanBytes(n int) Option { return func(g *Gossip) { g.maxOrphanBytes = n } }

// WithScorer wires a peer scorer: rejected messages penalise the sender, and a
// banned sender's blocks are dropped before decoding. Shared across
// gossip/txgossip/sync so misbehaviour on any of them counts.
func WithScorer(s *Scorer) Option { return func(g *Gossip) { g.scorer = s } }

// WithGapHandler wires the gap signal to a syncer. The handler must not block
// the gossip goroutine; a real one runs sync in its own goroutine.
func WithGapHandler(fn func(from PeerID)) Option { return func(g *Gossip) { g.onGap = fn } }
func WithLogger(l *log.Logger) Option {
	return func(g *Gossip) { g.log = l }
}

func NewGossip(net Network, bc *core.Blockchain, opts ...Option) (*Gossip, error) {
	g := &Gossip{
		net:           net,
		bc:            bc,
		maxOrphans:    DefaultMaxOrphans,
		orphans:       make(map[common.Hash][]orphanEntry),
		orphansByPeer: make(map[PeerID]int),
	}
	for _, o := range opts {
		o(g)
	}
	// A peer holds at most a quarter of the queue, so filling it takes four
	// colluding peers, each wasting only its own quota.
	if g.maxOrphansPerPeer == 0 {
		g.maxOrphansPerPeer = g.maxOrphans/4 + 1
	}
	if g.maxOrphanBytes == 0 {
		g.maxOrphanBytes = DefaultMaxOrphanBytes
	}
	if err := net.Subscribe(TopicBlocks, g.onBlock); err != nil {
		return nil, err
	}
	return g, nil
}

// Announce publishes a locally produced block. Called by the producer after the
// block is in the local chain: announcing first risks advertising a block that
// then fails to insert, and being asked for a block we do not have.
func (g *Gossip) Announce(b *types.Block) error {
	data, err := json.Marshal(blockMessage{Block: b})
	if err != nil {
		return err
	}
	return g.net.Publish(TopicBlocks, data)
}

// onBlock handles an inbound block. Hostile input is the default assumption.
func (g *Gossip) onBlock(from PeerID, data []byte) error {
	// A banned peer's traffic is dropped before it is measured or decoded: the
	// value of a ban is that the peer becomes cheap to ignore.
	if g.scorer != nil && g.scorer.Banned(from) {
		g.mu.Lock()
		g.Stats.Dropped++
		g.mu.Unlock()
		return nil
	}

	g.mu.Lock()
	g.Stats.Received++
	g.mu.Unlock()

	if len(data) > maxBlockMessage {
		g.penalize(from)
		return fmt.Errorf("%w: %d bytes from %s", ErrMessageTooLarge, len(data), from)
	}

	var msg blockMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		g.penalize(from)
		return fmt.Errorf("%w from %s: %v", ErrBadEncoding, from, err)
	}
	if msg.Block == nil || msg.Block.Header == nil {
		g.penalize(from)
		return fmt.Errorf("%w from %s: nil block", ErrBadEncoding, from)
	}

	blk := msg.Block

	// Cheap check first. GossipSub is a mesh: the same block arrives from every
	// meshed peer. Doing the expensive work before this makes a healthy network
	// look like an attack.
	if g.bc.HasBlock(blk.Hash()) {
		g.mu.Lock()
		g.Stats.FastDuplicate++
		g.mu.Unlock()
		return nil
	}

	return g.insert(blk, from)
}

// insert applies a block and resolves anything that was waiting for it.
func (g *Gossip) insert(blk *types.Block, from PeerID) error {
	err := g.bc.InsertBlock(blk)

	switch {
	case err == nil:
		g.mu.Lock()
		g.Stats.Accepted++
		g.mu.Unlock()
		// This block may be the parent some orphan is waiting for.
		g.resolveOrphans(blk.Hash())
		return nil

	case errors.Is(err, core.ErrKnownBlock):
		// Should be rare: HasBlock catches this earlier. The legitimate cause is
		// two peers delivering the same block concurrently.
		g.mu.Lock()
		g.Stats.LateDuplicate++
		g.mu.Unlock()
		return nil

	case errors.Is(err, core.ErrFutureBlock):
		// Not misbehaviour: the block is dated slightly ahead of our clock (NTP
		// skew, or faster propagation than a tick). Penalising the sender would
		// ban honest relayers over clock drift and fragment the mesh. Drop it
		// un-stored and un-penalised; re-gossip re-admits it once it falls inside
		// the drift window.
		g.mu.Lock()
		g.Stats.Deferred++
		g.mu.Unlock()
		return nil

	case errors.Is(err, core.ErrUnknownParent):
		// Not misbehaviour: gossip has no ordering, so a child beating its parent
		// is normal. Park it, then signal we may be behind. Parking resolves the
		// one-hop case without a round-trip; onGap covers a real multi-block gap
		// that gossip alone never closes. queueOrphan bounds the queue by COUNT, by
		// PER-PEER share, AND by total BYTES, so a parked orphan cannot exhaust memory
		// even though its real difficulty is unverifiable until its parent arrives.
		g.queueOrphan(blk, from)
		if g.onGap != nil {
			g.onGap(from)
		}
		return nil

	case errors.Is(err, core.ErrReorgTooDeep):
		// Not misbehaviour: the peer may be on a legitimately heavier fork that
		// diverged below our retention window. This is common right after a restart,
		// when the in-memory state window has not been rebuilt yet, so almost any fork
		// looks too deep. Banning the peer would isolate us on a losing branch. Drop
		// un-penalised.
		return nil

	default:
		// Genuinely invalid: bad state root, bad tx, bad PoW.
		g.penalize(from)
		return fmt.Errorf("p2p: rejected block %s from %s: %w", blk.Hash().Hex(), from, err)
	}
}

// queueOrphan parks a block until its parent shows up, charged to its sender.
func (g *Gossip) queueOrphan(blk *types.Block, from PeerID) {
	g.mu.Lock()
	parent := blk.Header.ParentHash

	// Do not queue the same block twice: gossip re-delivers it while its parent
	// is missing, and a queue that grows on duplicates has no bound at all.
	for _, existing := range g.orphans[parent] {
		if existing.blk.Hash() == blk.Hash() {
			g.mu.Unlock()
			return
		}
	}

	// Per-peer quota: a peer at its share cannot push more in by evicting other
	// peers' orphans. Its extra blocks are dropped and it is penalised for it.
	if g.orphansByPeer[from] >= g.maxOrphansPerPeer {
		g.mu.Unlock()
		g.penalize(from)
		return
	}

	size := orphanBytesOf(blk)

	// Evict oldest first (FIFO — the longest-waiting block is least likely to ever
	// resolve) until BOTH bounds admit the newcomer: the count bound and the byte
	// bound. The byte bound is what actually caps memory, since a peer can park
	// blocks claiming a trivial difficulty (the real difficulty is unverifiable
	// without the parent) each carrying up to a full message of calldata. Each
	// eviction refunds the evicted block to its own sender.
	for (g.orphanCount >= g.maxOrphans || g.orphanBytes+size > g.maxOrphanBytes) && len(g.orphanOrder) > 0 {
		oldest := g.orphanOrder[0]
		g.orphanOrder = g.orphanOrder[1:]
		for _, e := range g.orphans[oldest] {
			g.orphanCount--
			g.orphanBytes -= e.size
			g.orphansByPeer[e.from]--
			if g.orphansByPeer[e.from] <= 0 {
				delete(g.orphansByPeer, e.from)
			}
			g.Stats.Evicted++
		}
		delete(g.orphans, oldest)
	}

	// A single orphan larger than the whole byte budget can never fit even after
	// draining the queue; drop it rather than loop forever or blow the bound.
	if g.orphanBytes+size > g.maxOrphanBytes {
		g.mu.Unlock()
		g.penalize(from)
		return
	}

	if _, seen := g.orphans[parent]; !seen {
		g.orphanOrder = append(g.orphanOrder, parent)
	}
	g.orphans[parent] = append(g.orphans[parent], orphanEntry{blk: blk, from: from, size: size})
	g.orphansByPeer[from]++
	g.orphanCount++
	g.orphanBytes += size
	g.Stats.Orphaned++
	g.mu.Unlock()
}

// resolveOrphans applies any blocks waiting for parent, then anything waiting
// for those, and so on. Iterative, not recursive: the chain of waiting blocks
// is attacker-influenced, and recursing over attacker-controlled depth is a
// stack overflow.
func (g *Gossip) resolveOrphans(parent common.Hash) {
	queue := []common.Hash{parent}

	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]

		g.mu.Lock()
		waiting := g.orphans[h]
		if len(waiting) > 0 {
			delete(g.orphans, h)
			g.orphanCount -= len(waiting)
			for _, e := range waiting {
				g.orphanBytes -= e.size
				g.orphansByPeer[e.from]--
				if g.orphansByPeer[e.from] <= 0 {
					delete(g.orphansByPeer, e.from)
				}
			}
			for i, ph := range g.orphanOrder {
				if ph == h {
					g.orphanOrder = append(g.orphanOrder[:i], g.orphanOrder[i+1:]...)
					break
				}
			}
		}
		g.mu.Unlock()

		for _, e := range waiting {
			// Same validation path as any other block: waiting earns no trust.
			// If it is invalid, the peer that originally sent it is penalised.
			if err := g.bc.InsertBlock(e.blk); err != nil {
				if !errors.Is(err, core.ErrKnownBlock) {
					g.penalize(e.from)
					g.logf("rejected orphan %s from %s: %v", e.blk.Hash().Hex(), e.from, err)
				}
				continue
			}
			g.mu.Lock()
			g.Stats.Accepted++
			g.mu.Unlock()
			queue = append(queue, e.blk.Hash())
		}
	}
}

// penalize records a rejection and scores the peer that caused it, banning it
// once it crosses the threshold.
func (g *Gossip) penalize(from PeerID) {
	g.mu.Lock()
	g.Stats.Rejected++
	g.mu.Unlock()
	if g.scorer != nil {
		if g.scorer.Penalize(from, 1) {
			g.logf("p2p: banned peer %s (too many bad blocks)", from)
		}
	}
}

func (g *Gossip) logf(format string, args ...interface{}) {
	if g.log != nil {
		g.log.Printf(format, args...)
	}
}

// OrphanCount is the number of blocks currently parked. Test-only.
func (g *Gossip) OrphanCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.orphanCount
}

// OrphanBytes is the orphan queue's current approximate memory use, held under
// maxOrphanBytes. Exposed for tests and the health monitor.
func (g *Gossip) OrphanBytes() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.orphanBytes
}

// Snapshot returns a copy of the counters.
func (g *Gossip) Snapshot() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Stats
}
