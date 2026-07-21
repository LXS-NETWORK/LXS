// Package pool implements LXS pooled mining: many small machines combine their
// hashrate and split each block reward in proportion to contributed work, instead
// of one winner-takes-all lottery per block (solo mining stays available and is
// what a large miner should run).
//
// Protocol (deliberately NOT stratum): the pool is our own binary talking to our
// own binary, so the wire format is two JSON HTTP endpoints — GET /pool/work hands
// out the current block template plus an EASIER share target, POST /pool/share
// returns a nonce whose header-hash meets it. Both sides hash with the SAME
// core.Grind/core.PowTarget code the chain itself uses, so a share the pool
// accepts can never be block-invalid — reusing a foreign pool engine (stratum,
// ethash assumptions) is where hash-mismatch bugs live, which on an immutable
// chain would be unfixable in the field.
//
// Trust model, stated honestly: the pool is CUSTODIAL for the window between a
// block being won and payouts maturing. Workers trust the operator to credit
// shares and pay. What bounds the risk: payouts run automatically every maturity,
// balances persist to disk, and /pool/stats exposes the accounting publicly.
package pool

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"time"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

// Config carries the pool's economic knobs. Zero values select the defaults
// below — a caller only sets what it means to change.
type Config struct {
	// ShareDiffDivisor sets the share difficulty at blockDifficulty/divisor: a
	// share is that many times easier than a block, so a weak machine still
	// produces steady proof-of-contribution. 64 ≈ one share every few seconds
	// on a laptop at launch difficulty.
	ShareDiffDivisor uint64
	// MinShareDiff floors the share difficulty so a low-difficulty chain (tests,
	// young network) does not turn every hash into a share and flood the pool.
	MinShareDiff uint64
	// FeeBps is the pool operator's cut of each payout pot, in basis points.
	FeeBps uint64
	// Confirmations is how many blocks must build on a pool-won block before its
	// reward is credited to workers. A reorg deeper than this would orphan an
	// already-credited reward the pool no longer holds; 12 blocks (~48 min) is
	// far below retention (128) and deep enough for a young chain.
	Confirmations uint64
	// WindowFactor sizes the PPLNS window at factor×blockDifficulty of share
	// work. Pay-per-last-N-shares (the standard MiningCore default) rather than
	// per-round proportional, so hopping in only for lucky rounds earns nothing.
	WindowFactor uint64
	// PayoutMin is the balance at which the pool sends a payout tx. Batching
	// avoids paying 21000 gas on dust amounts.
	PayoutMin *big.Int
	// StatePath persists balances/pending blocks across restarts ("" = memory
	// only, for tests). Losing this file loses worker balances — it is the
	// pool's ledger.
	StatePath string
	// RefreshInterval rebuilds the current template even without a new head, so
	// its timestamp stays honest and new mempool txs get included.
	RefreshInterval time.Duration
}

func (c *Config) fillDefaults() {
	if c.ShareDiffDivisor == 0 {
		c.ShareDiffDivisor = 64
	}
	if c.MinShareDiff == 0 {
		c.MinShareDiff = 50_000
	}
	if c.Confirmations == 0 {
		c.Confirmations = 12
	}
	if c.WindowFactor == 0 {
		c.WindowFactor = 2
	}
	if c.PayoutMin == nil || c.PayoutMin.Sign() <= 0 {
		c.PayoutMin = new(big.Int).Div(common.OneLXS, big.NewInt(2)) // 0.5 LXS
	}
	if c.RefreshInterval == 0 {
		c.RefreshInterval = 10 * time.Second
	}
}

// payoutGasReserve is held back from each block's payout pot to fund the pool's
// own payout transactions (21000 gas each at gas price 1). 2M wei covers ~95
// payouts per block — invisible next to a 5×10^19 wei reward, but without it the
// pool wallet drifts negative by exactly the gas it spends paying people.
var payoutGasReserve = big.NewInt(2_000_000)

// template is one unit of distributable work: a fully-built block awaiting only
// its nonce. hdr is mutated under Server.mu to check shares (set nonce → hash);
// workJSON is the frozen /pool/work response so serving work never races that.
type template struct {
	id          common.Hash
	hdr         *types.Header
	txs         []*types.Transaction
	minerFees   *big.Int
	shareDiff   uint64
	shareTarget *big.Int
	blockTarget *big.Int
	workJSON    []byte
	built       time.Time
	parent      common.Hash
	seen        map[uint64]bool // nonces already credited on this template (dup guard)
}

// share is one accepted proof of contributed work in the PPLNS ring.
type share struct {
	addr common.Address
	diff uint64
}

// foundBlock is a pool-won block awaiting maturity. Payouts are snapshotted at
// find time (PPLNS over the ring as it was), then credited only once the block
// is Confirmations deep AND still canonical — crediting immediately would pay
// for a reward a reorg may take back.
type foundBlock struct {
	Height  uint64
	Hash    common.Hash
	Payouts map[common.Address]*big.Int
}

// Server is the pool engine: template manager, share validator, PPLNS ledger,
// payout engine, HTTP API. One mutex guards all accounting — share traffic is a
// few requests/second per worker, nowhere near mutex contention territory.
type Server struct {
	cfg       Config
	bc        *core.Blockchain
	prod      *core.Producer
	key       *crypto.PrivateKey
	mp        *mempool.Mempool
	broadcast func(*types.Transaction) error
	logf      func(string, ...any)

	mu        sync.Mutex
	templates map[common.Hash]*template
	current   common.Hash
	ring      []share // PPLNS ring, oldest first, bounded
	pending   []*foundBlock
	balances  map[common.Address]*big.Int
	lastSeen  map[common.Address]time.Time
	recent    []recentShare // for the public hashrate estimate

	nextNonce uint64 // pool-wallet payout nonce, faucet-style local tracking
	nonceInit bool

	totalShares  uint64
	totalBlocks  uint64
	totalOrphans uint64
	totalPaid    *big.Int
}

type recentShare struct {
	at   time.Time
	diff uint64
}

// maxRing bounds the PPLNS ring. At share diff = blockDiff/64 a window of
// 2×blockDiff is ~128 shares; 100k leaves orders of magnitude of headroom while
// capping memory against a misconfigured divisor.
const maxRing = 100_000

func NewServer(cfg Config, bc *core.Blockchain, prod *core.Producer, key *crypto.PrivateKey,
	mp *mempool.Mempool, broadcast func(*types.Transaction) error, logf func(string, ...any)) (*Server, error) {
	cfg.fillDefaults()
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{
		cfg: cfg, bc: bc, prod: prod, key: key, mp: mp, broadcast: broadcast, logf: logf,
		templates: map[common.Hash]*template{},
		balances:  map[common.Address]*big.Int{},
		lastSeen:  map[common.Address]time.Time{},
		totalPaid: new(big.Int),
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("pool: loading state: %w", err)
	}
	return s, nil
}

// Address is the pool wallet: block rewards land here and payouts leave from here.
func (s *Server) Address() common.Address { return s.key.Address() }

// Run drives the engine until ctx is done: keeps the template fresh, matures
// found blocks, and triggers payouts. The cadence (500ms) matches the solo
// producer's head-watcher — a stale template after a new block just wastes
// worker hashes on a doomed parent.
func (s *Server) Run(ctx interface{ Done() <-chan struct{} }) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	s.tick()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.saveLocked()
			s.mu.Unlock()
			return
		case <-t.C:
			s.tick()
		}
	}
}

func (s *Server) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := s.bc.Head()
	// Maturity/payout run EVERY tick, not only when this tick observes a head
	// move: handleShare refreshes the template the instant the pool wins a
	// block, so a "did the head move since my template?" check here can stay
	// false forever while pending blocks pile up unmatured. Cheap when idle.
	s.matureLocked(head.Header.Height)
	s.payoutLocked()
	cur := s.templates[s.current]
	headMoved := cur == nil || cur.parent != head.Hash()
	if headMoved || time.Since(cur.built) >= s.cfg.RefreshInterval {
		s.rebuildLocked(headMoved)
	}
}

// rebuildLocked builds a fresh template on the current head. When the head moved,
// every older template is on a dead parent — drop them all (this also drops their
// dup-nonce sets, which is what keeps `seen` bounded). On a same-parent refresh
// the old templates stay valid for in-flight shares.
func (s *Server) rebuildLocked(headMoved bool) {
	if headMoved {
		s.templates = map[common.Hash]*template{}
	}
	blk, fees, err := s.prod.BuildUnsealed()
	if err != nil {
		s.logf("pool: template build failed: %v", err)
		return
	}
	hdr := blk.Header
	shareDiff := hdr.Difficulty / s.cfg.ShareDiffDivisor
	if shareDiff < s.cfg.MinShareDiff {
		shareDiff = s.cfg.MinShareDiff
	}
	if shareDiff > hdr.Difficulty {
		shareDiff = hdr.Difficulty // a share may never be harder than the block
	}
	t := &template{
		id:          hdr.Hash(), // nonce=0 hash is unique per template
		hdr:         hdr,
		txs:         blk.Txs,
		minerFees:   fees,
		shareDiff:   shareDiff,
		shareTarget: core.PowTarget(shareDiff),
		blockTarget: core.PowTarget(hdr.Difficulty),
		built:       time.Now(),
		parent:      hdr.ParentHash,
		seen:        map[uint64]bool{},
	}
	work := map[string]any{
		"workId":      t.id.Hex(),
		"header":      json.RawMessage(mustJSON(hdr)),
		"shareTarget": "0x" + t.shareTarget.Text(16),
		"shareDiff":   shareDiff,
		"difficulty":  hdr.Difficulty,
		"height":      hdr.Height,
	}
	t.workJSON = mustJSON(work)
	// Cap the map: a same-parent refresh every RefreshInterval between 4-minute
	// blocks yields ~24 live templates; 64 is a hard stop, evicting nothing that
	// matters (the current template is always re-added).
	if len(s.templates) >= 64 {
		s.templates = map[common.Hash]*template{}
	}
	s.templates[t.id] = t
	s.current = t.id
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // marshaling our own structs cannot fail
	}
	return b
}

// handleShare validates one submitted nonce. The order is cheapest-first: lookup,
// dup, then one hash. A share meeting the BLOCK target is the pool winning a
// block — commit it, snapshot payouts, and roll the round.
func (s *Server) handleShare(workID common.Hash, nonce uint64, addr common.Address) (isBlock bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.templates[workID]
	if !ok {
		return false, errStale
	}
	if t.seen[nonce] {
		return false, errDuplicate
	}

	t.hdr.Nonce = nonce
	t.hdr.InvalidateHash()
	h := t.hdr.Hash()
	val := new(big.Int).SetBytes(h[:])
	if val.Cmp(t.shareTarget) > 0 {
		// Failing the easy target means the sender did not do the work it
		// claims — reject without credit.
		return false, errBadShare
	}

	t.seen[nonce] = true
	s.ring = append(s.ring, share{addr: addr, diff: t.shareDiff})
	if len(s.ring) > maxRing {
		s.ring = s.ring[len(s.ring)-maxRing:]
	}
	s.totalShares++
	now := time.Now()
	s.lastSeen[addr] = now
	if len(s.lastSeen) > 10_000 {
		for a, at := range s.lastSeen { // prune the dead; bounded map
			if now.Sub(at) > time.Hour {
				delete(s.lastSeen, a)
			}
		}
	}
	s.recent = append(s.recent, recentShare{at: now, diff: t.shareDiff})
	if len(s.recent) > 10_000 {
		s.recent = s.recent[len(s.recent)-10_000:]
	}

	if val.Cmp(t.blockTarget) > 0 {
		return false, nil // a plain share: credited, not a block
	}

	// Block found. Clone the header via JSON so later shares mutating t.hdr's
	// nonce cannot corrupt the committed block (Header carries a hash-cache
	// atomic.Value, so a struct copy is off the table — vet's copylocks).
	var clone types.Header
	if err := json.Unmarshal(mustJSON(t.hdr), &clone); err != nil {
		return false, fmt.Errorf("clone header: %w", err)
	}
	blk := &types.Block{Header: &clone, Txs: t.txs}
	if err := s.prod.Commit(blk); err != nil {
		// Lost a race with a competing block — the share still counted as work.
		s.logf("pool: block %d found but commit failed (race): %v", clone.Height, err)
		return false, nil
	}

	pot := new(big.Int).Add(state.BlockRewardAt(clone.Height), t.minerFees)
	if s.cfg.FeeBps > 0 {
		fee := new(big.Int).Mul(pot, new(big.Int).SetUint64(s.cfg.FeeBps))
		pot.Sub(pot, fee.Div(fee, big.NewInt(10000)))
	}
	pot.Sub(pot, payoutGasReserve)
	if pot.Sign() < 0 {
		pot.SetInt64(0)
	}
	fb := &foundBlock{
		Height:  clone.Height,
		Hash:    blk.Hash(),
		Payouts: s.pplnsLocked(pot, clone.Difficulty),
	}
	s.pending = append(s.pending, fb)
	s.totalBlocks++
	s.logf("pool: WON block %d %s — %d worker(s) in the payout window, matures at height %d",
		clone.Height, blk.Hash().Hex()[:12], len(fb.Payouts), clone.Height+s.cfg.Confirmations)
	s.saveLocked()
	s.rebuildLocked(true) // head moved: fresh work for everyone immediately
	return true, nil
}

// pplnsLocked splits pot across the last shares summing to WindowFactor×blockDiff
// of work (walking the ring newest-first). Integer-division dust stays in the
// pool wallet rather than being invented or lost.
func (s *Server) pplnsLocked(pot *big.Int, blockDiff uint64) map[common.Address]*big.Int {
	window := new(big.Int).SetUint64(blockDiff)
	window.Mul(window, new(big.Int).SetUint64(s.cfg.WindowFactor))

	perAddr := map[common.Address]*big.Int{}
	total := new(big.Int)
	for i := len(s.ring) - 1; i >= 0 && total.Cmp(window) < 0; i-- {
		sh := s.ring[i]
		d := new(big.Int).SetUint64(sh.diff)
		total.Add(total, d)
		if cur, ok := perAddr[sh.addr]; ok {
			cur.Add(cur, d)
		} else {
			perAddr[sh.addr] = d
		}
	}
	out := map[common.Address]*big.Int{}
	if total.Sign() == 0 {
		return out // no shares recorded — the whole pot stays with the pool
	}
	for addr, d := range perAddr {
		amt := new(big.Int).Mul(pot, d)
		amt.Div(amt, total)
		if amt.Sign() > 0 {
			out[addr] = amt
		}
	}
	return out
}

// matureLocked credits pending blocks that are Confirmations deep and still
// canonical. An orphaned block is dropped WITHOUT credit — the pool never
// received that reward, so paying it would be printing money the wallet lacks.
func (s *Server) matureLocked(headHeight uint64) {
	if len(s.pending) == 0 {
		return
	}
	kept := s.pending[:0]
	changed := false
	for _, fb := range s.pending {
		if headHeight < fb.Height+s.cfg.Confirmations {
			kept = append(kept, fb)
			continue
		}
		b, err := s.bc.BlockByHeight(fb.Height)
		if err != nil || b.Hash() != fb.Hash {
			s.totalOrphans++
			s.logf("pool: block %d %s was orphaned — reward not received, no credit", fb.Height, fb.Hash.Hex()[:12])
			changed = true
			continue
		}
		for addr, amt := range fb.Payouts {
			if cur, ok := s.balances[addr]; ok {
				cur.Add(cur, amt)
			} else {
				s.balances[addr] = new(big.Int).Set(amt)
			}
		}
		s.logf("pool: block %d matured — credited %d worker(s)", fb.Height, len(fb.Payouts))
		changed = true
	}
	s.pending = kept
	if changed {
		s.saveLocked()
	}
}

// payoutLocked sends one tx per worker whose balance crossed PayoutMin, exactly
// the faucet's dispensing pattern (local nonce tracking, CheckState before Add).
// The balance is deducted first and refunded on failure — the reverse order could
// double-pay on a crash between send and deduct.
func (s *Server) payoutLocked() {
	snap := s.bc.StateSnapshot()
	if committed := snap.Nonce(s.key.Address()); !s.nonceInit || committed > s.nextNonce {
		s.nextNonce = committed
		s.nonceInit = true
	}
	changed := false
	for addr, bal := range s.balances {
		if bal.Cmp(s.cfg.PayoutMin) < 0 {
			continue
		}
		amount := new(big.Int).Set(bal)
		to := addr
		tx := &types.Transaction{
			Type: types.TxTypeTransfer, ChainID: s.bc.ChainID(), Nonce: s.nextNonce,
			To: &to, Value: amount, GasLimit: 21000, GasPrice: big.NewInt(1),
		}
		if err := tx.Sign(s.key); err != nil {
			s.logf("pool: payout sign failed: %v", err)
			continue
		}
		if err := mempool.CheckState(snap, tx); err != nil {
			// The loud failure that matters: the pool wallet cannot cover what
			// its ledger owes. Never silent — this is worker money.
			s.logf("pool: PAYOUT BLOCKED for %s (%s wei): %v", addr.Hex(), amount, err)
			continue
		}
		if err := s.mp.Add(tx, s.bc.ChainID()); err != nil {
			s.logf("pool: payout tx rejected by mempool: %v", err)
			continue
		}
		if s.broadcast != nil {
			_ = s.broadcast(tx)
		}
		bal.SetInt64(0)
		delete(s.balances, addr)
		s.nextNonce++
		s.totalPaid.Add(s.totalPaid, amount)
		s.logf("pool: paid %s wei to %s (tx %s)", amount, addr.Hex(), tx.Hash().Hex()[:12])
		changed = true
	}
	if changed {
		s.saveLocked()
	}
}
