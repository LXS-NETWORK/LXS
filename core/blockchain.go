package core

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"lxs/common"
	"lxs/state"
	"lxs/store"
	"lxs/types"
)

var (
	ErrUnknownBlock     = errors.New("core: unknown block")
	ErrUnknownTx        = errors.New("core: unknown transaction")
	ErrUnknownParent    = errors.New("core: unknown parent")
	ErrKnownBlock       = errors.New("core: block already known")
	ErrNoCommonAncestor = errors.New("core: no common ancestor")
	ErrReorgTooDeep     = errors.New("core: reorg deeper than the retention window")
	ErrPrunedState      = errors.New("core: state pruned")
	ErrWrongChain       = errors.New("core: database belongs to a different chain")
	ErrBadDifficulty    = errors.New("core: block difficulty is not the LWMA-derived value")
	ErrBadPoW           = errors.New("core: block hash does not satisfy its difficulty target")
	// ErrFutureBlock is retriable, not invalid: refused and not stored, so it can
	// be re-offered once the clock catches up. The peer is not penalised.
	ErrFutureBlock = errors.New("core: block timestamp is too far in the future")
)

// MaxFutureDriftMs bounds how far ahead of local wall clock a block timestamp may
// be. Timestamps drive difficulty retargeting, so without a ceiling a miner could
// post-date a block to ease its own difficulty. A network-acceptance rule reading
// the validator's clock, so it lives here rather than in the deterministic
// ApplyBlock. 15s covers NTP skew; matches Ethereum's bound.
// This tight cap is load-bearing under LWMA: timestamps drive the weighted average, and
// while LWMA already clamps each solvetime to 6*T and enforces monotonicity, the 15s future
// ceiling keeps the amount a miner can post-date one block far below the target interval, so
// the influence on the windowed average is negligible. Do NOT widen this toward Bitcoin's 2h
// without first adding a median-time-past lower bound.
const MaxFutureDriftMs int64 = 15_000

// DefaultRetention is how many blocks back from head a materialised state is kept
// in memory. It is the finality assumption: a reorg deeper than this is rejected
// because the state to validate it against is no longer held.
const DefaultRetention = 128

type TxLocation struct {
	BlockHash   common.Hash `json:"blockHash"`
	BlockHeight uint64      `json:"blockHeight"`
	Index       uint64      `json:"index"`
}

type Reorg struct {
	CommonAncestor common.Hash
	Dropped        []*types.Block
	Added          []*types.Block
}

func (r *Reorg) Depth() int { return len(r.Dropped) }

// Blockchain is a persistent block tree.
//
// Disk holds blocks, receipts, tx index, canonical height map, head pointer, the
// account table at canonical head, and a reverse diff per canonical block so head
// can walk backwards. Memory holds materialised states within the retention window
// and a header cache; both bounded and pruned on every head change.
type Blockchain struct {
	mu sync.RWMutex

	db         store.KV
	chainID    uint64
	forkChoice ForkChoice
	genesis    common.Hash
	// genesisDifficulty is the difficulty for the first LwmaWindow blocks, before a full
	// LWMA averaging window exists. Cached so difficulty derivation needs no extra lookup.
	genesisDifficulty uint64
	retention         uint64
	// genesisSupply is the pre-mine total (Σ genesis alloc). With it, conservation
	// is checkable at any height: Σ balances + burned == genesisSupply + issued.
	genesisSupply *big.Int

	// Bounded caches over the disk source of truth, to avoid decoding JSON on
	// every access.
	blockCache map[common.Hash]*types.Block
	states     map[common.Hash]*state.State

	head *types.Block

	onReorg func(*Reorg)

	// now is the wall clock, injectable so tests drive the future-block rule
	// deterministically.
	now func() time.Time
}

type Options struct {
	ForkChoice ForkChoice
	// Retention is the reorg depth limit. 0 means DefaultRetention.
	Retention uint64
	// Now overrides the wall clock used for the future-block acceptance rule.
	// nil = time.Now.
	Now func() time.Time
}

// NewBlockchain opens or creates a chain in db. If db already holds this chain it
// resumes from the stored head; if it holds a different chain it refuses rather
// than run a node against another network's database.
func NewBlockchain(db store.KV, g *Genesis, opts Options) (*Blockchain, error) {
	if opts.ForkChoice == nil {
		opts.ForkChoice = HeaviestChain{}
	}
	if opts.Retention == 0 {
		opts.Retention = DefaultRetention
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// Validate before touching the database.
	if err := g.Validate(); err != nil {
		return nil, err
	}

	// Load the consensus reward-split parameters into the state package (read by
	// CreditBlockReward). Every node reads the same genesis, so the split cannot
	// diverge. Set on both the init and resume paths.
	state.TreasuryRewardBasisPoints = int64(g.TreasuryRewardBps)
	state.TreasuryRewardAddress = g.TreasuryReward

	genesisState, genesisBlock := g.Build()
	bc := &Blockchain{
		db:                db,
		chainID:           g.ChainID,
		forkChoice:        opts.ForkChoice,
		genesis:           genesisBlock.Hash(),
		genesisDifficulty: genesisBlock.Header.Difficulty,
		retention:         opts.Retention,
		blockCache:        make(map[common.Hash]*types.Block),
		states:            make(map[common.Hash]*state.State),
		now:               opts.Now,
		genesisSupply:     genesisState.SumBalances(), // constant: the pre-mine total
	}

	existing, err := db.Has(keyHead)
	if err != nil {
		return nil, err
	}
	if existing {
		if err := bc.resume(); err != nil {
			return nil, err
		}
		return bc, nil
	}
	if err := bc.initialise(g); err != nil {
		return nil, err
	}
	return bc, nil
}

// NewMemBlockchain is a convenience for tests and the demo.
func NewMemBlockchain(g *Genesis) *Blockchain {
	bc, err := NewBlockchain(store.NewMemory(), g, Options{})
	if err != nil {
		panic(err)
	}
	return bc
}

func (bc *Blockchain) initialise(g *Genesis) error {
	genesisState, genesisBlock := g.Build()

	b := bc.db.NewBatch()
	putUint64(b, keySchema, schemaVersion)
	putUint64(b, keyChainID, g.ChainID)
	putHash(b, keyGenesis, genesisBlock.Hash())

	if err := writeBlock(b, genesisBlock, nil); err != nil {
		return err
	}
	putHash(b, canonicalKey(0), genesisBlock.Hash())
	// Genesis carries its own difficulty as the base of accumulated work.
	putTD(b, genesisBlock.Hash(), new(big.Int).SetUint64(genesisBlock.Header.Difficulty))

	// The genesis allocation goes through the same diff machinery as any state
	// change.
	genesisState.ClearTouched()
	for addr := range g.Alloc {
		acc := genesisState.Get(addr)
		data, err := encodeJSON(acc)
		if err != nil {
			return err
		}
		b.Put(accountKey(addr), data)
	}
	empty, err := encodeJSON(storedDiff{})
	if err != nil {
		return err
	}
	b.Put(revDiffKey(genesisBlock.Hash()), empty)

	// Head goes in the same batch as everything it names.
	putHash(b, keyHead, genesisBlock.Hash())

	if err := b.Commit(); err != nil {
		return err
	}

	bc.head = genesisBlock
	bc.blockCache[genesisBlock.Hash()] = genesisBlock
	bc.states[genesisBlock.Hash()] = genesisState
	return nil
}

// resume rebuilds the in-memory view from a database that already has one.
func (bc *Blockchain) resume() error {
	version, err := getUint64(bc.db, keySchema)
	if err != nil {
		return fmt.Errorf("core: unreadable schema version: %w", err)
	}
	if version != schemaVersion {
		return fmt.Errorf("core: schema version %d, this build expects %d", version, schemaVersion)
	}

	// Guard against opening a database from a different network.
	storedGenesis, err := getHash(bc.db, keyGenesis)
	if err != nil {
		return err
	}
	if storedGenesis != bc.genesis {
		return fmt.Errorf("%w: database genesis %s, config genesis %s",
			ErrWrongChain, storedGenesis.Hex(), bc.genesis.Hex())
	}
	storedChainID, err := getUint64(bc.db, keyChainID)
	if err != nil {
		return err
	}
	if storedChainID != bc.chainID {
		return fmt.Errorf("%w: database chain id %d, config chain id %d",
			ErrWrongChain, storedChainID, bc.chainID)
	}

	headHash, err := getHash(bc.db, keyHead)
	if err != nil {
		return err
	}
	head, err := loadBlock(bc.db, headHash)
	if err != nil {
		return fmt.Errorf("core: head %s is unreadable: %w", headHash.Hex(), err)
	}

	// The account table on disk IS the state at head.
	st, err := loadState(bc.db)
	if err != nil {
		return err
	}
	// Restore the burn total before the root check: Root() commits to it, so a zero
	// burn total would root differently than head claimed. Absent key = nothing burned.
	burned, err := getBigInt(bc.db, keyBurned)
	if err != nil {
		return err
	}
	st.SetBurned(burned)
	// The state rebuilt from disk must hash to the root head claimed; a mismatch
	// means the database is inconsistent and the node must not run.
	if root := st.Root(); root != head.Header.StateRoot {
		return fmt.Errorf("core: state loaded from disk roots to %s, head says %s — database is inconsistent",
			root.Hex(), head.Header.StateRoot.Hex())
	}

	bc.head = head
	bc.blockCache[headHash] = head
	bc.states[headHash] = st

	// Rebuild the in-memory reorg window so a restarted node can follow a fork that
	// diverged within retention (and does not wrongly refuse/ban the peer offering it).
	bc.rebuildWindow(head, st)

	// Side branches are not restored; a restarted node re-learns them from peers.
	return nil
}

// rebuildWindow reconstructs the in-memory states of up to `retention` canonical
// ancestors by replaying their reverse diffs backward from the head. Without it a
// restarted node holds only the head state, so its effective reorg window is zero
// until it mines/receives `retention` fresh blocks — during which it cannot adopt a
// heavier fork that diverged in the window.
//
// Correctness is guarded by a per-step root check: each reconstructed state must
// reproduce its block's committed StateRoot before it is trusted. A mismatch stops
// the walk (the window is simply smaller, never wrong), so a reconstruction bug can
// never seed a corrupt state that a later reorg would build on. Best-effort: any
// missing diff/block just ends the rebuild.
func (bc *Blockchain) rebuildWindow(head *types.Block, headState *state.State) {
	cur, curBlock := headState, head
	for i := uint64(0); i < bc.retention && curBlock.Height() > 0; i++ {
		diff, err := loadStoredDiff(bc.db, curBlock.Hash())
		if err != nil {
			return
		}
		parentBlock, err := loadBlock(bc.db, curBlock.Header.ParentHash)
		if err != nil {
			return
		}
		parent := cur.Copy()
		for _, e := range diff.Entries {
			if !e.Existed {
				parent.RestoreAccount(e.Address, 0, nil, nil, nil) // empty -> deleted
			} else {
				parent.RestoreAccount(e.Address, e.Nonce, e.Balance, e.Storage, e.Code)
			}
		}
		if diff.BurnDelta != nil {
			parent.SetBurned(new(big.Int).Sub(cur.Burned(), diff.BurnDelta))
		}
		parent.ClearTouched()
		if parent.Root() != parentBlock.Header.StateRoot {
			return // reconstruction disagrees with the committed root: stop, do not store it
		}
		bc.states[parentBlock.Hash()] = parent
		bc.blockCache[parentBlock.Hash()] = parentBlock
		cur, curBlock = parent, parentBlock
	}
}

func (bc *Blockchain) SetReorgHook(fn func(*Reorg)) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.onReorg = fn
}

func (bc *Blockchain) ChainID() uint64        { return bc.chainID }
func (bc *Blockchain) ForkChoice() ForkChoice { return bc.forkChoice }
func (bc *Blockchain) Retention() uint64      { return bc.retention }

func (bc *Blockchain) Head() *types.Block {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.head
}

func (bc *Blockchain) StateSnapshot() *state.State {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.states[bc.head.Hash()].Copy()
}

// Read-only head accessors. Unlike StateSnapshot they do NOT clone the whole world:
// the stored head state is never mutated in place after being set (each block yields a
// fresh copy), so a read under the lock is safe. This matters on the RPC hot path — a
// MetaMask balance poll must not O(all-accounts)-copy the entire state on every call,
// which would collapse the node exactly when adoption (many accounts) arrives.

// BalanceAt returns a COPY of an account's balance (so a caller cannot mutate state).
func (bc *Blockchain) BalanceAt(addr common.Address) *big.Int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return new(big.Int).Set(bc.states[bc.head.Hash()].Balance(addr))
}

// NonceAt returns an account's committed nonce at the head.
func (bc *Blockchain) NonceAt(addr common.Address) uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.states[bc.head.Hash()].Nonce(addr)
}

// CodeAt returns a contract's runtime code at the head (read-only; callers must not mutate).
func (bc *Blockchain) CodeAt(addr common.Address) []byte {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.states[bc.head.Hash()].GetCode(addr)
}

// StorageAt returns one storage slot at the head.
func (bc *Blockchain) StorageAt(addr common.Address, key common.Hash) common.Hash {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.states[bc.head.Hash()].GetStorage(addr, key)
}

// CheckConservation verifies value conservation at the head:
// Σ balances + burned == genesisSupply + issued. Conservation holds structurally
// (only ApplyTx/CreditBlockReward/Burn move value), so a violation means a
// deterministic mint/burn bug — which every node computes identically, so it inflates
// supply silently with no root divergence to catch it. This is the loud local check
// (Cosmos's invariant halt) that turns silent inflation into a reported error.
// O(accounts); run periodically, not per block.
func (bc *Blockchain) CheckConservation() error {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	st := bc.states[bc.head.Hash()]
	have := new(big.Int).Add(st.SumBalances(), st.Burned())
	want := new(big.Int).Add(bc.genesisSupply, state.CumulativeIssued(bc.head.Height()))
	if have.Cmp(want) != 0 {
		return fmt.Errorf("core: value conservation VIOLATED at height %d: balances+burned=%s, genesis+issued=%s",
			bc.head.Height(), have, want)
	}
	return nil
}

// Call runs a read-only eth_call against a copy of the head state, so contract
// reads mutate nothing; any SSTORE is discarded with the copy.
func (bc *Blockchain) Call(from, to common.Address, data []byte, gas uint64) ([]byte, error) {
	return state.Call(bc.StateSnapshot(), from, to, data, gas)
}

// EstimateGas approximates the gas a message would use, against the head state.
func (bc *Blockchain) EstimateGas(from common.Address, to *common.Address, data []byte, value *big.Int) (uint64, error) {
	return state.EstimateGas(bc.StateSnapshot(), from, to, data, value, 30_000_000)
}

// StateAt returns the state at a known block, or ErrPrunedState for a block
// outside the retention window.
func (bc *Blockchain) StateAt(h common.Hash) (*state.State, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	s, ok := bc.states[h]
	if !ok {
		if _, err := bc.blockLocked(h); err == nil {
			return nil, fmt.Errorf("%w: %s is outside the %d-block retention window",
				ErrPrunedState, h.Hex(), bc.retention)
		}
		return nil, ErrUnknownBlock
	}
	return s.Copy(), nil
}

func (bc *Blockchain) HasBlock(h common.Hash) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if _, ok := bc.blockCache[h]; ok {
		return true
	}
	ok, _ := bc.db.Has(blockKey(h))
	return ok
}

// blockLocked reads a block from cache, falling back to disk.
func (bc *Blockchain) blockLocked(h common.Hash) (*types.Block, error) {
	if b, ok := bc.blockCache[h]; ok {
		return b, nil
	}
	b, err := loadBlock(bc.db, h)
	if err == store.ErrNotFound {
		return nil, ErrUnknownBlock
	}
	if err != nil {
		return nil, err
	}
	bc.blockCache[h] = b
	return b, nil
}

func (bc *Blockchain) BlockByHash(h common.Hash) (*types.Block, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.blockLocked(h)
}

// requiredDifficultyLocked is the difficulty a child of parent must carry: LWMA over the
// last LwmaWindow blocks ending at parent. Before a full window exists (parent below the
// window size) it holds the genesis difficulty. It walks back via ParentHash, so a block is
// scored on its OWN ancestry (a fork's history), not the canonical chain. LWMA does not use
// the child's own timestamp, so a block's required difficulty is fixed by its ancestors.
// Caller must hold bc.mu (uses the unlocked blockLocked); InsertBlock already holds it.
func (bc *Blockchain) requiredDifficultyLocked(parent *types.Header) uint64 {
	const N = LwmaWindow
	const minWindow = 2 // retarget from block 3 (needs 2 solvetimes); before that, hold genesis
	// Retarget over the largest window available: the full LwmaWindow once the chain
	// is that tall, or the whole chain so far while it is younger. This is what lets
	// a fresh chain drop from an over-set genesis difficulty to the real hashrate
	// within a few blocks, instead of stalling at genesis until block LwmaWindow.
	w := int(parent.Height)
	if w > N {
		w = N
	}
	if w < minWindow {
		return bc.genesisDifficulty
	}
	window := make([]*types.Header, w+1)
	window[w] = parent
	cur := parent
	for i := w - 1; i >= 0; i-- {
		b, err := bc.blockLocked(cur.ParentHash)
		if err != nil {
			// A linked block always retains its ancestors within the window; a miss
			// means we cannot derive LWMA, so fall back to the floor rather than guess.
			return bc.genesisDifficulty
		}
		window[i] = b.Header
		cur = b.Header
	}
	return lwmaDifficulty(window)
}

// RequiredDifficulty is the self-locking form for callers outside a locked section (the
// block producer). Returns the difficulty a child of parent must carry.
func (bc *Blockchain) RequiredDifficulty(parent *types.Header) uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.requiredDifficultyLocked(parent)
}

func (bc *Blockchain) BlockByHeight(n uint64) (*types.Block, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	h, err := getHash(bc.db, canonicalKey(n))
	if err == store.ErrNotFound {
		return nil, ErrUnknownBlock
	}
	if err != nil {
		return nil, err
	}
	return bc.blockLocked(h)
}

func (bc *Blockchain) IsCanonical(h common.Hash) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	b, err := bc.blockLocked(h)
	if err != nil {
		return false
	}
	got, err := getHash(bc.db, canonicalKey(b.Height()))
	if err != nil {
		return false
	}
	return got == h
}

func (bc *Blockchain) TxByHash(h common.Hash) (*types.Transaction, TxLocation, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	data, err := bc.db.Get(txIndexKey(h))
	if err == store.ErrNotFound {
		return nil, TxLocation{}, ErrUnknownTx
	}
	if err != nil {
		return nil, TxLocation{}, err
	}
	var loc TxLocation
	if err := decodeJSON(data, &loc); err != nil {
		return nil, TxLocation{}, err
	}
	blk, err := bc.blockLocked(loc.BlockHash)
	if err != nil {
		return nil, TxLocation{}, err
	}
	if int(loc.Index) >= len(blk.Txs) {
		return nil, TxLocation{}, ErrUnknownTx
	}
	return blk.Txs[loc.Index], loc, nil
}

// ReceiptsByHeight returns the canonical block at height n and its receipts, for
// eth_getLogs. receipt[i] belongs to block.Txs[i].
func (bc *Blockchain) ReceiptsByHeight(n uint64) ([]*types.Receipt, *types.Block, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	h, err := getHash(bc.db, canonicalKey(n))
	if err == store.ErrNotFound {
		return nil, nil, ErrUnknownBlock
	}
	if err != nil {
		return nil, nil, err
	}
	blk, err := bc.blockLocked(h)
	if err != nil {
		return nil, nil, err
	}
	rs, err := loadReceipts(bc.db, blk.Hash())
	if err != nil {
		return nil, nil, err
	}
	return rs, blk, nil
}

func (bc *Blockchain) ReceiptByTxHash(h common.Hash) (*types.Receipt, TxLocation, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	data, err := bc.db.Get(txIndexKey(h))
	if err == store.ErrNotFound {
		return nil, TxLocation{}, ErrUnknownTx
	}
	if err != nil {
		return nil, TxLocation{}, err
	}
	var loc TxLocation
	if err := decodeJSON(data, &loc); err != nil {
		return nil, TxLocation{}, err
	}
	rs, err := loadReceipts(bc.db, loc.BlockHash)
	if err != nil {
		return nil, TxLocation{}, ErrUnknownTx
	}
	if int(loc.Index) >= len(rs) {
		return nil, TxLocation{}, ErrUnknownTx
	}
	return rs[loc.Index], loc, nil
}

// InsertBlock validates a block and, if the fork choice prefers it, makes it the
// new head, persisting everything atomically.
func (bc *Blockchain) InsertBlock(b *types.Block) error {
	// The unlock is deferred, NOT straight-line: insert() does hash/validation work
	// (VerifyTxRoot, tx sanity) that must not be able to leave bc.mu locked forever.
	// A single gossiped block with a nil tx once panicked under this lock; the gossip
	// firewall recovered the process, but the never-released mutex then deadlocked
	// every later Head()/InsertBlock()/RPC read — one message froze the whole node.
	// The hook must still fire OUTSIDE the lock (it re-enters the chain), so the
	// locked section is scoped to this closure.
	reorg, hook, err := func() (*Reorg, func(*Reorg), error) {
		bc.mu.Lock()
		defer bc.mu.Unlock()
		reorg, err := bc.insert(b)
		return reorg, bc.onReorg, err
	}()

	if err != nil {
		return err
	}
	if reorg != nil && hook != nil {
		hook(reorg)
	}
	return nil
}

func (bc *Blockchain) insert(b *types.Block) (*Reorg, error) {
	h := b.Hash()
	if known, _ := bc.db.Has(blockKey(h)); known {
		return nil, ErrKnownBlock
	}
	if _, cached := bc.blockCache[h]; cached {
		return nil, ErrKnownBlock
	}

	// Reject a nil tx element BEFORE anything touches the tx slice — including the
	// orphan path (an unknown-parent block is parked by p2p, which sizes its txs).
	// A gossiped/decoded body can carry {"txs":[null]}, which unmarshals to a nil
	// *Transaction; VerifyTxRoot -> tx.Hash(), SanityCheck, and the orphan sizer all
	// nil-deref and panic. One such block once panicked under bc.mu and, with the
	// old straight-line Unlock, poisoned the mutex and froze the whole node.
	for _, tx := range b.Txs {
		if tx == nil {
			return nil, fmt.Errorf("core: block %s carries a nil transaction", b.Hash().Hex())
		}
	}

	parent, err := bc.blockLocked(b.Header.ParentHash)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownParent, b.Header.ParentHash.Hex())
	}

	// Reject a block dated too far ahead, before the expensive PoW check. Refused
	// cheaply and not stored, so it can be re-offered once time catches up.
	if nowMs := bc.now().UnixMilli(); b.Header.Timestamp > nowMs+MaxFutureDriftMs {
		return nil, fmt.Errorf("%w: %d is %dms past now %d (max drift %d)",
			ErrFutureBlock, b.Header.Timestamp, b.Header.Timestamp-nowMs, nowMs, MaxFutureDriftMs)
	}

	// Proof of work in two checks. Difficulty first: it is derived from the parent,
	// so a block claiming any other value is invalid before its nonce is checked.
	// Then the nonce must satisfy that difficulty's target; both are required, since
	// a proof against a self-lowered difficulty is worthless. Timestamp (which the
	// derivation depends on) is validated strictly-increasing in ApplyBlock.
	if want := bc.requiredDifficultyLocked(parent.Header); b.Header.Difficulty != want {
		return nil, fmt.Errorf("%w: header says %d, LWMA over the window derives %d", ErrBadDifficulty, b.Header.Difficulty, want)
	}
	if !satisfiesPoW(b.Header) {
		return nil, fmt.Errorf("%w: %s at difficulty %d", ErrBadPoW, b.Hash().Hex(), b.Header.Difficulty)
	}

	// Bind the body to the PoW-committed TxRoot BEFORE recovering signatures. A junk
	// body swapped onto a reused valid header fails this O(hash) check, so it is
	// rejected before the O(n) EC-recovery loop below (~50µs/tx) it would otherwise
	// trigger — closing a cheap CPU-amplification vector.
	if !b.VerifyTxRoot() {
		return nil, fmt.Errorf("%w: %s", state.ErrBadTxRoot, b.Hash().Hex())
	}
	for _, tx := range b.Txs {
		if err := tx.SanityCheck(bc.chainID); err != nil {
			return nil, fmt.Errorf("core: invalid tx %s: %w", tx.Hash().Hex(), err)
		}
	}

	parentState, ok := bc.states[parent.Hash()]
	if !ok {
		// Parent known but its state pruned: a reorg from before the retention
		// window, refused.
		return nil, fmt.Errorf("%w: parent %s is %d blocks behind head",
			ErrReorgTooDeep, parent.Hash().Hex(), bc.head.Height()-parent.Height())
	}

	newState, receipts, err := state.ApplyBlock(parentState, b, parent.Header)
	if err != nil {
		return nil, err
	}
	// Touched is exactly what this block changed: ApplyBlock clears it after
	// copying the parent state.
	touched := newState.Touched()

	// Accumulated work is the parent's plus this block's; the fork choice weighs
	// work, not height.
	parentTD, err := getTD(bc.db, parent.Hash())
	if err != nil {
		return nil, fmt.Errorf("core: total difficulty for parent %s: %w", parent.Hash().Hex(), err)
	}
	td := new(big.Int).Add(parentTD, new(big.Int).SetUint64(b.Header.Difficulty))

	headTD, err := getTD(bc.db, bc.head.Hash())
	if err != nil {
		return nil, fmt.Errorf("core: total difficulty for head %s: %w", bc.head.Hash().Hex(), err)
	}

	if !bc.forkChoice.Better(&Tip{b.Header, td}, &Tip{bc.head.Header, headTD}) {
		// Valid but losing: persist the block, its state, and its work so it can
		// win later once descendants arrive. No canonical changes, no head move.
		batch := bc.db.NewBatch()
		if err := writeBlock(batch, b, receipts); err != nil {
			return nil, err
		}
		putTD(batch, h, td)
		if err := batch.Commit(); err != nil {
			return nil, err
		}
		bc.blockCache[h] = b
		bc.states[h] = newState
		return nil, nil
	}

	if b.Header.ParentHash == bc.head.Hash() {
		if err := bc.extend(b, receipts, newState, touched, td); err != nil {
			return nil, err
		}
		return nil, nil
	}

	return bc.reorg(b, receipts, newState, td)
}

// extend commits a block that builds directly on head. Everything lands in one
// batch, so a crash mid-write leaves the node on the old head with nothing
// half-applied.
func (bc *Blockchain) extend(b *types.Block, receipts []*types.Receipt, newState *state.State, touched []common.Address, td *big.Int) error {
	batch := bc.db.NewBatch()

	if err := writeBlock(batch, b, receipts); err != nil {
		return err
	}
	putTD(batch, b.Hash(), td)
	prev, ok := bc.states[b.Header.ParentHash]
	if !ok {
		return fmt.Errorf("%w: no parent state for %s", ErrPrunedState, b.Hash().Hex())
	}
	if err := writeAccountChanges(batch, b, newState, prev, touched); err != nil {
		return err
	}
	putHash(batch, canonicalKey(b.Height()), b.Hash())
	if err := indexTxs(batch, b); err != nil {
		return err
	}
	putHash(batch, keyHead, b.Hash())
	// Burn total stored absolute (not diffed) so disk always matches the canonical
	// head, through extend and reorg.
	putBigInt(batch, keyBurned, newState.Burned())

	if err := batch.Commit(); err != nil {
		return err
	}

	bc.blockCache[b.Hash()] = b
	bc.states[b.Hash()] = newState
	bc.head = b
	bc.prune()
	return nil
}

// reorg moves the canonical chain to a new head on a different branch.
func (bc *Blockchain) reorg(newHead *types.Block, receipts []*types.Receipt, newState *state.State, td *big.Int) (*Reorg, error) {
	oldHead := bc.head

	dropped, added, ancestor, err := bc.findFork(oldHead, newHead)
	if err != nil {
		return nil, err
	}
	if uint64(len(dropped)) > bc.retention {
		return nil, fmt.Errorf("%w: %d blocks (limit %d)", ErrReorgTooDeep, len(dropped), bc.retention)
	}

	batch := bc.db.NewBatch()

	// The new tip's own data must be on disk before anything points at it.
	if err := writeBlock(batch, newHead, receipts); err != nil {
		return nil, err
	}
	putTD(batch, newHead.Hash(), td)

	// Unwind newest first; reverse diffs restore the values each orphaned block
	// overwrote.
	for i := len(dropped) - 1; i >= 0; i-- {
		blk := dropped[i]
		if err := applyReverseDiff(bc.db, batch, blk.Hash()); err != nil {
			return nil, err
		}
		for _, tx := range blk.Txs {
			batch.Delete(txIndexKey(tx.Hash()))
		}
		batch.Delete(canonicalKey(blk.Height()))
	}

	// Apply the new branch oldest first; a replay of diffs recorded when the blocks
	// were first validated, not a re-execution.
	for _, blk := range added {
		// The new tip's state is not in bc.states yet: it is published only after
		// this batch commits, so a failed write never leaves a state for a block
		// absent from disk. Handle it explicitly before the map lookup.
		var st *state.State
		if blk.Hash() == newHead.Hash() {
			st = newState
		} else {
			var ok bool
			st, ok = bc.states[blk.Hash()]
			if !ok {
				return nil, fmt.Errorf("%w: no state for %s on the winning branch", ErrPrunedState, blk.Hash().Hex())
			}
		}
		prev, ok := bc.states[blk.Header.ParentHash]
		if !ok {
			return nil, fmt.Errorf("%w: no parent state for %s on the winning branch", ErrPrunedState, blk.Hash().Hex())
		}
		if err := writeAccountChanges(batch, blk, st, prev, st.Touched()); err != nil {
			return nil, err
		}
		putHash(batch, canonicalKey(blk.Height()), blk.Hash())
		if err := indexTxs(batch, blk); err != nil {
			return nil, err
		}
	}

	putHash(batch, keyHead, newHead.Hash())
	// Overwrite disk with the new head's burn total, so a restart reads the reorged
	// supply, not the abandoned branch's.
	putBigInt(batch, keyBurned, newState.Burned())

	if err := batch.Commit(); err != nil {
		return nil, err
	}

	bc.blockCache[newHead.Hash()] = newHead
	bc.states[newHead.Hash()] = newState
	bc.head = newHead
	bc.prune()

	return &Reorg{CommonAncestor: ancestor, Dropped: dropped, Added: added}, nil
}

// prune drops in-memory state outside the retention window. Blocks stay on disk;
// a state older than the window can never be reorged onto, so holding it is pure
// cost. Reverse diffs for canonical blocks are not pruned: they let head walk
// backwards.
func (bc *Blockchain) prune() {
	if bc.head.Height() <= bc.retention {
		return
	}
	cutoff := bc.head.Height() - bc.retention

	for h, st := range bc.states {
		blk, ok := bc.blockCache[h]
		if !ok {
			// State without its block: drop it rather than leak.
			delete(bc.states, h)
			_ = st
			continue
		}
		if blk.Height() < cutoff {
			delete(bc.states, h)
		}
	}
	for h, blk := range bc.blockCache {
		if blk.Height() < cutoff && h != bc.genesis {
			// Only the cache entry goes; the block stays on disk and is read
			// back on demand.
			delete(bc.blockCache, h)
		}
	}

	// Periodically sweep orphaned branch data off disk (not every block: the sweep
	// scans the block table). A block below cutoff that is not canonical at its
	// height lost a fork and can never be reorged to again.
	if bc.head.Height()%GCInterval == 0 {
		bc.gcOrphans(cutoff)
	}
}

// GCInterval is how often, in canonical blocks, orphaned branch data is swept off
// disk. A var so tests can lower it; kept high in production to keep the table
// scan rare.
var GCInterval uint64 = 4096

// gcOrphans deletes the on-disk data (block, receipts, reverse diff, total
// difficulty) of every block below cutoff that is not canonical at its height.
// Such a block lost a fork below retention and can never be reorged to again.
// Canonical chain, genesis, and the retention window are untouched.
//
// Collect-then-delete: the block table is scanned and the iterator closed before
// any delete, so the store is never mutated under an open iterator.
func (bc *Blockchain) gcOrphans(cutoff uint64) {
	it := bc.db.Iterate([]byte{prefixBlock})
	type bh struct {
		hash   common.Hash
		height uint64
	}
	var below []bh
	for it.Next() {
		key := it.Key()
		if len(key) != 1+common.HashLength {
			continue
		}
		var h common.Hash
		copy(h[:], key[1:])
		if h == bc.genesis {
			continue
		}
		var blk types.Block
		if decodeJSON(it.Value(), &blk) != nil {
			continue
		}
		if blk.Header.Height < cutoff {
			below = append(below, bh{h, blk.Header.Height})
		}
	}
	it.Close()

	batch := bc.db.NewBatch()
	deleted := 0
	for _, x := range below {
		canon, err := getHash(bc.db, canonicalKey(x.height))
		if err == nil && canon == x.hash {
			continue // canonical block at its height: keep it
		}
		// Orphan below retention: remove every trace. Tx-index entries were dropped
		// when the reorg unwound it.
		batch.Delete(blockKey(x.hash))
		batch.Delete(receiptsKey(x.hash))
		batch.Delete(revDiffKey(x.hash))
		batch.Delete(tdKey(x.hash))
		deleted++
	}
	if deleted > 0 {
		_ = batch.Commit()
	}
}

func (bc *Blockchain) findFork(oldHead, newHead *types.Block) (dropped, added []*types.Block, ancestor common.Hash, err error) {
	a, b := oldHead, newHead

	for a.Height() > b.Height() {
		dropped = append(dropped, a)
		if a, err = bc.parentOf(a); err != nil {
			return nil, nil, common.ZeroHash, err
		}
	}
	for b.Height() > a.Height() {
		added = append(added, b)
		if b, err = bc.parentOf(b); err != nil {
			return nil, nil, common.ZeroHash, err
		}
	}
	for a.Hash() != b.Hash() {
		if a.Height() == 0 {
			return nil, nil, common.ZeroHash, ErrNoCommonAncestor
		}
		dropped = append(dropped, a)
		added = append(added, b)
		if a, err = bc.parentOf(a); err != nil {
			return nil, nil, common.ZeroHash, err
		}
		if b, err = bc.parentOf(b); err != nil {
			return nil, nil, common.ZeroHash, err
		}
	}

	reverse(dropped)
	reverse(added)
	return dropped, added, a.Hash(), nil
}

func (bc *Blockchain) parentOf(b *types.Block) (*types.Block, error) {
	p, err := bc.blockLocked(b.Header.ParentHash)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownParent, b.Header.ParentHash.Hex())
	}
	return p, nil
}

func indexTxs(batch store.Batch, b *types.Block) error {
	for i, tx := range b.Txs {
		data, err := encodeJSON(TxLocation{
			BlockHash:   b.Hash(),
			BlockHeight: b.Height(),
			Index:       uint64(i),
		})
		if err != nil {
			return err
		}
		batch.Put(txIndexKey(tx.Hash()), data)
	}
	return nil
}

// Close releases the database.
func (bc *Blockchain) Close() error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.db.Close()
}

// StateCount reports how many materialised states are held in memory. Test-only.
func (bc *Blockchain) StateCount() int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return len(bc.states)
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
