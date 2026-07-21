package p2p

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"lxs/common"
	"lxs/core"
	"lxs/types"
)

// maxSyncMessage caps a single sync request or response. Mirrors
// maxRequestMessage in the libp2p adapter; the cap must hold on both transports.
const maxSyncMessage = 16 << 20

// maxHeadersPerRequest bounds one headers answer. A syncing node walks forward
// in batches, not the whole chain at once. The bound stops one request forcing
// an unbounded marshal on the server, and stops a node being handed more headers
// than it asked for. A peer returning more is refused whole.
const maxHeadersPerRequest = 256

var (
	ErrBadSyncRequest  = errors.New("p2p: malformed sync request")
	ErrHeaderChain     = errors.New("p2p: headers do not form a chain")
	ErrHeadersUnlinked = errors.New("p2p: headers do not connect to our chain")
	// ErrFutureHeader: a header dated too far ahead of our clock. Not a lie — an
	// honest peer whose freshly-mined tip is a few seconds ahead of ours hits this,
	// exactly as block gossip does (where it is a non-penalised "deferred"). So sync
	// must NOT penalise it either; it stops without a score hit and retries once the
	// clock catches up. Kept distinct from ErrHeaderChain (deliberate garbage) so the
	// caller can tell honest drift from a malformed batch.
	ErrFutureHeader = errors.New("p2p: header dated too far in the future")
	ErrBodyMismatch = errors.New("p2p: body does not match its header")
)

// syncKind discriminates the two round-trips of headers-first sync.
type syncKind string

const (
	kindHeaders syncKind = "headers" // give me canonical headers from a height
	kindBodies  syncKind = "bodies"  // give me the full blocks for these hashes
)

// syncRequest is one question asked of one peer. JSON, like the gossip wire, and
// safe for the same reason: nothing in it is trusted to be canonical, only to
// round-trip. Every header and block it leads to is re-hashed and revalidated
// before it can move our head.
type syncRequest struct {
	Kind   syncKind      `json:"kind"`
	From   uint64        `json:"from,omitempty"`   // headers: first canonical height wanted
	Max    uint64        `json:"max,omitempty"`    // headers: how many (server clamps)
	Hashes []common.Hash `json:"hashes,omitempty"` // bodies: which blocks
}

type syncResponse struct {
	Headers []*types.Header `json:"headers,omitempty"`
	Blocks  []*types.Block  `json:"blocks,omitempty"`
}

// Syncer is headers-first block sync, the request/response counterpart to
// gossip. Gossip cannot backfill: a node that missed early blocks orphans
// everything after them forever. Sync fetches headers, validates the chain
// cheaply, then fetches bodies for what checks out and inserts them through the
// same InsertBlock everything else uses.
//
// Headers first: a peer feeding a fake chain is caught at the header stage (a
// few kilobytes and hash links) before any body bandwidth or state execution is
// spent on it.
// Sync requests are far heavier than tx gossip — each can trigger up to
// maxHeadersPerRequest block reads and a multi-MiB marshal — so the server rate-limits
// them per peer. A peer that exceeds it is answered with an error, not served.
const (
	defaultSyncRate  = 20 // sustained sync requests/sec per peer
	defaultSyncBurst = 40
)

type Syncer struct {
	net     Network
	bc      *core.Blockchain
	log     *log.Logger
	scorer  *Scorer
	limiter *txRateLimiter // per-peer rate limit on inbound sync requests

	// One sync at a time. The trigger (an orphan off the gossip path) fires once
	// per stray block, a burst on a real gap; without this, a burst would launch
	// overlapping syncs all fetching the same range. A dropped trigger is
	// harmless: the next orphan re-arms it.
	mu      sync.Mutex
	syncing bool
}

type SyncOption func(*Syncer)

func WithSyncLogger(l *log.Logger) SyncOption { return func(s *Syncer) { s.log = l } }

// WithSyncScorer shares a peer scorer with sync: a peer that answers with a
// chain that does not link, or bodies that do not match their headers, is
// penalised toward a ban.
func WithSyncScorer(sc *Scorer) SyncOption { return func(s *Syncer) { s.scorer = sc } }

// NewSyncer registers the server side and returns the client driver.
func NewSyncer(net Network, bc *core.Blockchain, opts ...SyncOption) (*Syncer, error) {
	s := &Syncer{net: net, bc: bc}
	for _, o := range opts {
		o(s)
	}
	if s.limiter == nil {
		s.limiter = newTxRateLimiter(defaultSyncRate, defaultSyncBurst, nil)
	}
	if err := net.SetRequestHandler(ProtoSync, s.serve); err != nil {
		return nil, err
	}
	return s, nil
}

// serve answers a peer's sync request from our canonical chain. Read-only: a
// server never mutates state on behalf of a peer's question.
func (s *Syncer) serve(from PeerID, data []byte) ([]byte, error) {
	if s.limiter != nil && !s.limiter.allow(from) {
		return nil, fmt.Errorf("%w: rate limit exceeded", ErrBadSyncRequest)
	}
	if len(data) > maxSyncMessage {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadSyncRequest, len(data))
	}
	var req syncRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSyncRequest, err)
	}

	var resp syncResponse
	switch req.Kind {
	case kindHeaders:
		n := req.Max
		if n == 0 || n > maxHeadersPerRequest {
			n = maxHeadersPerRequest
		}
		for i := uint64(0); i < n; i++ {
			blk, err := s.bc.BlockByHeight(req.From + i)
			if err != nil {
				break // ran off the end of what we have; that is the answer
			}
			resp.Headers = append(resp.Headers, blk.Header)
		}
	case kindBodies:
		if len(req.Hashes) > maxHeadersPerRequest {
			return nil, fmt.Errorf("%w: asked for %d bodies", ErrBadSyncRequest, len(req.Hashes))
		}
		// Bound the response by BYTES, not just block count: 256 large blocks could marshal
		// far past maxSyncMessage (which only the client enforces), so a peer could force an
		// unbounded server-side build+send it then discards. Stop well under the cap.
		approx := 0
		for _, h := range req.Hashes {
			blk, err := s.bc.BlockByHash(h)
			if err != nil {
				continue // we do not have it; the caller copes with a gap
			}
			sz := 1024
			for _, tx := range blk.Txs {
				sz += len(tx.Data) + 384
			}
			if len(resp.Blocks) > 0 && approx+sz > maxSyncMessage*3/4 {
				break // conservative headroom for JSON overhead; the client re-requests the rest
			}
			approx += sz
			resp.Blocks = append(resp.Blocks, blk)
		}
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrBadSyncRequest, req.Kind)
	}

	return json.Marshal(resp)
}

// SyncFrom brings us up to date with one peer. Blocks until caught up or
// stalled, so callers that must not block (the gossip handler) run it in a
// goroutine. Safe to call concurrently: the second caller returns immediately
// while the first runs.
func (s *Syncer) SyncFrom(peer PeerID) {
	s.mu.Lock()
	if s.syncing {
		s.mu.Unlock()
		return
	}
	s.syncing = true
	s.mu.Unlock()
	defer func() {
		// SyncFrom runs in a bare goroutine, so an unrecovered panic here (e.g. a future
		// decode bug on a hostile response) would take down the whole node. Recover, so a
		// malicious peer can at worst abort one sync round, and always release the slot.
		if r := recover(); r != nil {
			log.Printf("p2p: recovered panic during sync from %s: %v", peer, r)
		}
		s.mu.Lock()
		s.syncing = false
		s.mu.Unlock()
	}()

	// Do not sync from a peer we have already banned for lying.
	if s.scorer != nil && s.scorer.Banned(peer) {
		return
	}

	for {
		start := s.bc.Head().Height() + 1

		headers, err := s.getHeaders(peer, start)
		if err != nil {
			s.logf("sync from %s: headers: %v", peer, err)
			return
		}
		if len(headers) == 0 {
			return // the peer has nothing past our head: caught up
		}
		if err := s.checkHeaderChain(headers, start); err != nil {
			if errors.Is(err, ErrFutureHeader) {
				// Honest clock drift, not misbehaviour — gossip treats the same
				// condition as a non-penalised "deferred". Stop without a score hit;
				// the tip is re-offered once our clock catches up.
				s.logf("sync from %s: %v (deferred, no penalty)", peer, err)
				return
			}
			if !errors.Is(err, ErrHeadersUnlinked) {
				// A batch starting at the wrong height or not internally linking
				// is deliberate garbage: score it and stop, rather than loop
				// asking the same liar the same question.
				s.penalize(peer)
				s.logf("sync from %s: %v", peer, err)
				return
			}
			// The headers do not attach at our head: the peer is on a fork that
			// diverged below it. Not a lie. Find the common ancestor and re-fetch
			// from there so HeaviestChain can reorg us onto the peer's chain if it
			// is heavier. No penalty on this path.
			anc, ok, aerr := s.findCommonAncestor(peer, start-1)
			if aerr != nil {
				s.logf("sync from %s: ancestor search: %v", peer, aerr)
				return
			}
			if !ok {
				s.logf("sync from %s: fork below the retention window (%d) — cannot reorg", peer, s.bc.Retention())
				return
			}
			start = anc + 1
			headers, err = s.getHeaders(peer, start)
			if err != nil {
				s.logf("sync from %s: headers from ancestor %d: %v", peer, anc, err)
				return
			}
			if len(headers) == 0 {
				return
			}
			if err := s.checkHeaderChain(headers, start); err != nil {
				// From the common ancestor the headers must link. If they still
				// do not, the peer is lying: now it earns the penalty.
				s.penalize(peer)
				s.logf("sync from %s: %v", peer, err)
				return
			}
		}

		blocks, err := s.getBodies(peer, headers)
		if err != nil {
			// A body not matching its header is the peer lying; a plain transport
			// failure is not. Only the former earns a penalty.
			if errors.Is(err, ErrBodyMismatch) {
				s.penalize(peer)
			}
			s.logf("sync from %s: bodies: %v", peer, err)
			return
		}

		newly := 0
		for _, b := range blocks {
			err := s.bc.InsertBlock(b)
			switch {
			case err == nil:
				newly++ // a genuinely new block: real progress
			case errors.Is(err, core.ErrKnownBlock):
				// Already held: a re-pulled fork block. Not progress; if a whole
				// round is only these, the loop below terminates.
			case errors.Is(err, core.ErrFutureBlock):
				// The peer's chain is ahead of our clock. Stop pulling; the block
				// arrives via gossip and is admitted once the clock catches up. No
				// penalty: clock skew is not a lie.
				s.logf("sync from %s: block %s is ahead of our clock; deferring", peer, b.Hash().Hex())
				return
			case errors.Is(err, core.ErrReorgTooDeep):
				// The peer's fork diverged below our retention window (common just
				// after a restart, before the window is rebuilt). Not a lie — we simply
				// cannot follow it. Stop pulling, do NOT penalise; isolating an honest
				// peer on the heavier chain would be worse.
				s.logf("sync from %s: fork below retention at %s; cannot follow", peer, b.Hash().Hex())
				return
			default:
				// The header checked out but a body failed full validation (bad
				// state/tx root). The peer lied below the header. Stop and score it.
				s.penalize(peer)
				s.logf("sync from %s: insert %s: %v", peer, b.Hash().Hex(), err)
				return
			}
		}
		if newly == 0 {
			// No new blocks this round: caught up, or we pulled the peer's entire
			// fork and HeaviestChain did not prefer it. Looping would re-pull the
			// same known blocks forever.
			return
		}
		// Head advanced (or a side branch grew and may yet win). Loop for the next
		// batch, in case the peer is further ahead than one batch carries.
	}
}

// getHeaders asks the peer for canonical headers starting at `start`.
func (s *Syncer) getHeaders(peer PeerID, start uint64) ([]*types.Header, error) {
	req, err := json.Marshal(syncRequest{Kind: kindHeaders, From: start, Max: maxHeadersPerRequest})
	if err != nil {
		return nil, err
	}
	data, err := s.net.Request(peer, ProtoSync, req)
	if err != nil {
		return nil, err
	}
	if len(data) > maxSyncMessage {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadSyncRequest, len(data))
	}
	var resp syncResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if len(resp.Headers) > maxHeadersPerRequest {
		return nil, fmt.Errorf("%w: %d headers", ErrHeaderChain, len(resp.Headers))
	}
	// A peer can encode a nil element (`{"headers":[null]}`); it survives the length
	// check and would nil-deref in checkHeaderChain. Reject it like any malformed batch
	// (getBodies already guards its slice the same way).
	for _, h := range resp.Headers {
		if h == nil {
			return nil, fmt.Errorf("%w: nil header in batch", ErrHeaderChain)
		}
	}
	return resp.Headers, nil
}

// getHeaderAt fetches the peer's single canonical header at one height, the
// probe the common-ancestor binary search uses.
func (s *Syncer) getHeaderAt(peer PeerID, height uint64) (*types.Header, error) {
	req, err := json.Marshal(syncRequest{Kind: kindHeaders, From: height, Max: 1})
	if err != nil {
		return nil, err
	}
	data, err := s.net.Request(peer, ProtoSync, req)
	if err != nil {
		return nil, err
	}
	if len(data) > maxSyncMessage {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadSyncRequest, len(data))
	}
	var resp syncResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if len(resp.Headers) == 0 {
		return nil, fmt.Errorf("%w: peer has no header at %d", ErrHeaderChain, height)
	}
	if resp.Headers[0] == nil {
		return nil, fmt.Errorf("%w: nil header at %d", ErrHeaderChain, height)
	}
	if resp.Headers[0].Height != height {
		return nil, fmt.Errorf("%w: asked height %d, got %d", ErrHeaderChain, height, resp.Headers[0].Height)
	}
	return resp.Headers[0], nil
}

// agreesAt reports whether the peer's canonical header at height is the exact
// block we hold there, i.e. whether the two chains still share history there.
func (s *Syncer) agreesAt(peer PeerID, height uint64) (bool, error) {
	ph, err := s.getHeaderAt(peer, height)
	if err != nil {
		return false, err
	}
	ours, err := s.bc.BlockByHeight(height)
	if err != nil {
		return false, err
	}
	return ph.Hash() == ours.Hash(), nil
}

// findCommonAncestor binary-searches the highest height at which the peer's
// chain still matches ours: the block to reorg from when the peer is on a fork
// that diverged below our head. Bounded below by the retention window, since a
// fork whose common ancestor is older than that cannot be reorged to (we no
// longer hold the state to validate it) — it returns ok=false rather than walk
// to genesis. A fork is not misbehaviour, so this path never penalises the peer.
func (s *Syncer) findCommonAncestor(peer PeerID, head uint64) (ancestor uint64, ok bool, err error) {
	var lo uint64
	if head > s.bc.Retention() {
		lo = head - s.bc.Retention()
	}
	// The search needs a known-common floor. If we do not agree even at lo, the
	// fork is deeper than we can follow: refuse rather than reorg blind.
	agree, err := s.agreesAt(peer, lo)
	if err != nil || !agree {
		return 0, false, err
	}
	// Invariant: agree at lo, differ at head. Binary-search the boundary; lo ends
	// on the highest agreeing height, the common ancestor.
	hi := head
	for lo+1 < hi {
		mid := (lo + hi) / 2
		agree, err := s.agreesAt(peer, mid)
		if err != nil {
			return 0, false, err
		}
		if agree {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, true, nil
}

// checkHeaderChain is the cheap gate before any body is fetched. Without
// touching state it proves the batch starts where we asked, each header is the
// parent of the next, and the first header builds on a block we hold. Any
// failure refuses the whole chain, so bodies are never fetched for headers that
// do not link.
func (s *Syncer) checkHeaderChain(headers []*types.Header, start uint64) error {
	if headers[0].Height != start {
		return fmt.Errorf("%w: batch starts at %d, asked %d", ErrHeaderChain, headers[0].Height, start)
	}
	// The batch must attach to a block we hold at start-1, or it is a chain from a
	// network we are not on (or a fork we cannot follow yet).
	anchor, err := s.bc.BlockByHeight(start - 1)
	if err != nil {
		return fmt.Errorf("%w: no block at %d to attach to", ErrHeadersUnlinked, start-1)
	}
	if headers[0].ParentHash != anchor.Hash() {
		return fmt.Errorf("%w: first header's parent %s is not our block %s",
			ErrHeadersUnlinked, headers[0].ParentHash.Hex(), anchor.Hash().Hex())
	}
	// Validate linkage AND proof-of-work for the whole batch before any body fetch.
	// A linked header chain is free to forge, but each header that also satisfies the
	// difficulty it claims required real mining work — so this rejects a fabricated chain
	// in hash time instead of after a multi-MiB body download. (The exact LWMA difficulty
	// is re-derived authoritatively in InsertBlock, which holds the full window.)
	_ = anchor
	for i := 0; i < len(headers); i++ {
		if i > 0 {
			if headers[i].Height != headers[i-1].Height+1 {
				return fmt.Errorf("%w: height jump %d -> %d", ErrHeaderChain, headers[i-1].Height, headers[i].Height)
			}
			if headers[i].ParentHash != headers[i-1].Hash() {
				return fmt.Errorf("%w: header %d is not the child of %d", ErrHeaderChain, headers[i].Height, headers[i-1].Height)
			}
		}
		// Reject a header dated absurdly far ahead of our clock before spending work on it
		// (mirrors InsertBlock's ErrFutureBlock). Retriable — a slightly-ahead honest tip is
		// re-offered once the clock catches up; a far-future timestamp is a hostile input.
		if headers[i].Timestamp > time.Now().UnixMilli()+core.MaxFutureDriftMs {
			return fmt.Errorf("%w: header %d", ErrFutureHeader, headers[i].Height)
		}
		if err := core.VerifyHeaderPoW(headers[i]); err != nil {
			return fmt.Errorf("%w: %v", ErrHeaderChain, err)
		}
	}
	return nil
}

// getBodies fetches the full blocks for validated headers, in header order,
// verifying each hashes to the header it answers. A hash mismatch is a
// bait-and-switch (valid headers advertised, different blocks delivered). Stops
// at the first gap so the caller inserts a contiguous run.
func (s *Syncer) getBodies(peer PeerID, headers []*types.Header) ([]*types.Block, error) {
	want := make([]common.Hash, len(headers))
	byHash := make(map[common.Hash]*types.Header, len(headers))
	for i, h := range headers {
		hash := h.Hash()
		want[i] = hash
		byHash[hash] = h
	}

	req, err := json.Marshal(syncRequest{Kind: kindBodies, Hashes: want})
	if err != nil {
		return nil, err
	}
	data, err := s.net.Request(peer, ProtoSync, req)
	if err != nil {
		return nil, err
	}
	if len(data) > maxSyncMessage {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadSyncRequest, len(data))
	}
	var resp syncResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	have := make(map[common.Hash]*types.Block, len(resp.Blocks))
	for _, b := range resp.Blocks {
		if b == nil || b.Header == nil {
			return nil, fmt.Errorf("%w: nil block", ErrBodyMismatch)
		}
		h := b.Hash()
		if _, wanted := byHash[h]; !wanted {
			return nil, fmt.Errorf("%w: got block %s we did not ask for", ErrBodyMismatch, h.Hex())
		}
		have[h] = b
	}

	ordered := make([]*types.Block, 0, len(want))
	for _, h := range want {
		b, ok := have[h]
		if !ok {
			break // first gap: insert the contiguous prefix, re-request the rest next round
		}
		ordered = append(ordered, b)
	}
	return ordered, nil
}

// penalize scores a peer that answered a sync request with garbage.
func (s *Syncer) penalize(peer PeerID) {
	if s.scorer != nil {
		if s.scorer.Penalize(peer, 1) {
			s.logf("p2p: banned peer %s (bad sync responses)", peer)
		}
	}
}

func (s *Syncer) logf(format string, args ...interface{}) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}
