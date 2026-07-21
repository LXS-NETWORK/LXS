package state

import (
	"bytes"
	"math/big"
	"sort"

	"lxs/common"
	"lxs/types"
)

// Account is a world-state leaf. Balance model, not UTXO: contracts need
// persistent storage, which UTXO does not map to cleanly.
type Account struct {
	Nonce   uint64   `json:"nonce"`
	Balance *big.Int `json:"balance"`

	// Storage holds a contract's persistent slots; nil for a plain account. A
	// zero value is never stored (unset == zero), keeping the encoding and state
	// root canonical.
	Storage map[common.Hash]common.Hash `json:"storage,omitempty"`

	// Code is the deployed bytecode, empty for an externally owned account. It
	// folds into the account hash so the state root commits to a contract's
	// code; otherwise two nodes could run different code at one address.
	Code []byte `json:"code,omitempty"`
}

func (a *Account) copy() *Account {
	c := &Account{Nonce: a.Nonce, Balance: new(big.Int).Set(a.Balance)}
	if len(a.Storage) > 0 {
		c.Storage = make(map[common.Hash]common.Hash, len(a.Storage))
		for k, v := range a.Storage {
			c.Storage[k] = v
		}
	}
	if len(a.Code) > 0 {
		c.Code = append([]byte(nil), a.Code...)
	}
	return c
}

func (a *Account) encode() []byte {
	e := common.NewEncoder()
	e.Uint64(a.Nonce)
	e.BigInt(a.Balance)
	// Storage folds into the account hash so any SSTORE changes the state root.
	// Keys are sorted because Go randomises map iteration.
	// Count-prefixed (even at zero): without a boundary a (storage, no-code) and
	// a (no-storage, code) account can be crafted to encode identically, the code
	// length-prefix masquerading as a storage slot. The count keeps the account
	// commitment injective.
	keys := make([]common.Hash, 0, len(a.Storage))
	for k := range a.Storage {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	e.Uint64(uint64(len(keys)))
	for _, k := range keys {
		e.Raw(k.Bytes())
		e.Raw(a.Storage[k].Bytes())
	}
	// Code commits into the hash too, separated from storage by the count above.
	if len(a.Code) > 0 {
		e.Bytes(a.Code)
	}
	return e.Done()
}

// State is the world state at one point in the chain. Every account is held in
// memory and the root is recomputed from scratch (O(n) per block); a Merkle
// Patricia Trie is the eventual replacement. The simple version doubles as a
// reference the trie can be validated against.
type State struct {
	accounts map[common.Address]*Account

	// touched records every address written since the last ClearTouched, so
	// persistence writes a diff instead of the whole world each block. It is
	// maintained by set()/Credit() as writes happen rather than derived from the
	// tx list, which breaks once a contract can touch arbitrary accounts.
	touched map[common.Address]bool

	// snapshots is the stack of savepoints the VM pushes per call frame; a
	// reverting sub-call pops back to its savepoint. Backed by the journal below,
	// so a savepoint is a marker, not a state copy.
	snapshots []*snapshot

	// journal is the undo log behind the snapshot stack: each account write made
	// while a snapshot is active appends its prior value, and a revert replays the
	// tail in reverse. Cleared on commit (DiscardSnapshots). Writes with no active
	// snapshot (e.g. the up-front gas charge) are never journaled.
	journal []journalEntry

	// logs is a flat list of events emitted during execution; ApplyTx slices out
	// the window one tx produced. Logs ride the snapshot stack, so a reverted
	// call's events vanish with its writes.
	logs []*common.Log

	// bctx carries the block environment (number, time, difficulty) to the VM for
	// NUMBER/TIMESTAMP/DIFFICULTY. Producer and validators set it from the same
	// header so the values, and the resulting root, match. Not consensus state,
	// just a carrier held here to avoid threading it through ApplyTx.
	bctx blockContext

	// burned is the cumulative value the protocol has destroyed, removed from
	// supply and held by no account. It is consensus state: it folds into Root(),
	// so a node that miscounts burns computes a different root and is rejected.
	// Rides Copy() and the snapshot stack like a balance. nil is treated as zero
	// so an older-constructed State stays valid.
	burned *big.Int
}

type blockContext struct {
	number     uint64
	time       uint64 // seconds (Ethereum's block.timestamp unit)
	difficulty *big.Int
}

// SetBlockContext records the block environment for the next block's txs. Call
// once per block before applying them.
func (s *State) SetBlockContext(number, timeSeconds, difficulty uint64) {
	s.bctx = blockContext{number: number, time: timeSeconds, difficulty: new(big.Int).SetUint64(difficulty)}
}

// snapshot is a savepoint marker: where the journal and log list stood and the
// burn total at that point. A revert replays the journal back to journalLen and
// restores the scalars, so Snapshot/Revert is O(writes-since-savepoint).
type snapshot struct {
	journalLen int
	logCount   int
	burned     *big.Int
}

// journalEntry is one undoable account write. prev is the pointer stored before
// this write (nil if absent); existed and wasTouched record the prior presence of
// the account and its touched flag. Mutators copy an account before set() swaps
// the pointer, so prev is never mutated in place and restoring it is exact.
type journalEntry struct {
	addr       common.Address
	prev       *Account
	existed    bool
	wasTouched bool
}

func New() *State {
	return &State{
		accounts: make(map[common.Address]*Account),
		touched:  make(map[common.Address]bool),
		burned:   new(big.Int),
	}
}

// Copy returns an independent snapshot. Reorgs apply a block to a copy and
// discard it if the block is invalid or orphaned, rather than undoing writes.
func (s *State) Copy() *State {
	out := &State{
		accounts: make(map[common.Address]*Account, len(s.accounts)),
		touched:  make(map[common.Address]bool, len(s.touched)),
	}
	for addr, acc := range s.accounts {
		out.accounts[addr] = acc.copy()
	}
	// touched carries over: the producer copies the working state per tx, and a
	// copy that forgot earlier writes would produce a diff omitting them.
	for addr := range s.touched {
		out.touched[addr] = true
	}
	// Logs carry over for the same reason: ApplyTx's log window is only correct
	// if the copy remembers how many logs earlier txs emitted.
	out.logs = append([]*common.Log(nil), s.logs...)
	// Block context carries over so a per-tx copy knows its block.
	out.bctx = s.bctx
	// Burn total carries over, or the block's final root omits burns from earlier
	// txs and validators reject it.
	out.burned = new(big.Int).Set(s.burnedOrZero())
	return out
}

// burnedOrZero reads the burn total, treating nil as zero.
func (s *State) burnedOrZero() *big.Int {
	if s.burned == nil {
		return new(big.Int)
	}
	return s.burned
}

// ClearTouched resets the change set at the start of a block, so Touched()
// afterwards means changed by this block.
func (s *State) ClearTouched() {
	s.touched = make(map[common.Address]bool)
}

// Touched returns the addresses written since the last ClearTouched.
func (s *State) Touched() []common.Address {
	out := make([]common.Address, 0, len(s.touched))
	for addr := range s.touched {
		out = append(out, addr)
	}
	// Sorted: this feeds a database batch, and an unsorted diff makes two nodes
	// write the same block in different orders.
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i][:], out[j][:]) < 0
	})
	return out
}

// Exists reports whether an account has any state. Persistence needs it: an
// emptied account must be deleted from disk, not stored as zeroes, or the root
// computed from disk will not match the in-memory root.
func (s *State) Exists(addr common.Address) bool {
	_, ok := s.accounts[addr]
	return ok
}

func (s *State) Get(addr common.Address) *Account {
	if acc, ok := s.accounts[addr]; ok {
		return acc
	}
	// Non-existent and zero-balance are the same, so the root does not depend on
	// whether an empty account was ever touched (an early-Ethereum bug).
	return &Account{Nonce: 0, Balance: new(big.Int)}
}

func (s *State) set(addr common.Address, acc *Account) {
	// Journal the prior value for O(1) undo, but only while a savepoint is active;
	// writes outside any snapshot are permanent.
	if len(s.snapshots) > 0 {
		prev, existed := s.accounts[addr]
		s.journal = append(s.journal, journalEntry{addr: addr, prev: prev, existed: existed, wasTouched: s.touched[addr]})
	}
	s.touched[addr] = true
	// Delete only a fully empty account: no nonce, balance, storage, or code. A
	// contract with a zero balance but live storage or code still exists.
	if acc.Nonce == 0 && acc.Balance.Sign() == 0 && len(acc.Storage) == 0 && len(acc.Code) == 0 {
		delete(s.accounts, addr)
		return
	}
	s.accounts[addr] = acc
}

// Balance returns a COPY: s.Get(addr).Balance is the live pointer inside the stored
// account, so handing it out raw lets a caller that mutates it silently corrupt consensus
// state and diverge the root. A defensive copy removes that footgun for negligible cost.
func (s *State) Balance(addr common.Address) *big.Int { return new(big.Int).Set(s.Get(addr).Balance) }
func (s *State) Nonce(addr common.Address) uint64     { return s.Get(addr).Nonce }

// RestoreAccount sets an account to exact fields, deleting it if fully empty (set
// handles that). It exists for replaying a reverse diff into memory during the
// restart window-rebuild; call it outside any snapshot. balance is copied; storage
// and code are taken as given (the caller passes freshly-decoded values).
func (s *State) RestoreAccount(addr common.Address, nonce uint64, balance *big.Int, storage map[common.Hash]common.Hash, code []byte) {
	bal := new(big.Int)
	if balance != nil {
		bal.Set(balance)
	}
	s.set(addr, &Account{Nonce: nonce, Balance: bal, Storage: storage, Code: code})
}

// SumBalances totals every account balance. O(accounts) — used only by the periodic
// conservation self-check, not the hot path.
func (s *State) SumBalances() *big.Int {
	total := new(big.Int)
	for _, acc := range s.accounts {
		total.Add(total, acc.Balance)
	}
	return total
}

// CumulativeIssued is the total block reward minted from height 1 through h,
// accounting for halving. It MUST equal Σ_{i=1}^{h} BlockRewardAt(i) exactly —
// it is the expected-issuance term in CheckConservation, so any drift from the real
// per-block ledger raises a false "conservation VIOLATED" alarm. Closed-form over
// eras (a handful of terms even after centuries), so it is cheap regardless of height.
//
// The subtlety that a naive interval*reward-per-era sum gets wrong: BlockRewardAt
// halves AT the boundary (height 1,000,000 already pays 25, Bitcoin-canonical), and
// height 0 is genesis and pays no block reward. So era 0 has only HalvingInterval-1
// reward-bearing heights (1..999,999), while every later era e spans the full
// [e*I, e*I+I-1]. Counting era 0 as a full interval over-counts the first halving
// block by half a reward, and the gap widens each era — which at ~7.6 years
// post-launch would make every node log a false CRITICAL forever on an immutable chain.
func CumulativeIssued(h uint64) *big.Int {
	total := new(big.Int)
	for era := uint64(0); ; era++ {
		start := era * HalvingInterval // first height of this era
		if start > h {
			break
		}
		reward := BlockRewardAt(start)
		if reward.Sign() == 0 {
			break // issuance has ended; no more is added
		}
		lo := start
		if lo == 0 {
			lo = 1 // height 0 is genesis: allocated, not mined, so no block reward
		}
		hi := start + HalvingInterval - 1 // last height of this era
		if hi > h {
			hi = h
		}
		if hi < lo {
			continue
		}
		total.Add(total, new(big.Int).Mul(reward, new(big.Int).SetUint64(hi-lo+1)))
	}
	return total
}

// GetNonce is Nonce under the name vm.StateDB expects (CREATE derivation).
func (s *State) GetNonce(addr common.Address) uint64 { return s.Nonce(addr) }

// GetStorage returns a contract's storage slot, or the zero hash if unset.
func (s *State) GetStorage(addr common.Address, key common.Hash) common.Hash {
	acc, ok := s.accounts[addr]
	if !ok {
		return common.Hash{}
	}
	return acc.Storage[key]
}

// SetStorage writes a slot. Writing the zero hash clears the slot rather than
// storing zero (unset == zero), keeping the encoding canonical.
func (s *State) SetStorage(addr common.Address, key, value common.Hash) {
	acc := s.Get(addr).copy()
	if value.IsZero() {
		delete(acc.Storage, key)
	} else {
		if acc.Storage == nil {
			acc.Storage = make(map[common.Hash]common.Hash)
		}
		acc.Storage[key] = value
	}
	s.set(addr, acc)
}

// GetCode returns a contract's deployed bytecode, or nil for a plain account.
func (s *State) GetCode(addr common.Address) []byte {
	acc, ok := s.accounts[addr]
	if !ok {
		return nil
	}
	return acc.Code
}

// SetCode installs a contract's bytecode at deployment time.
func (s *State) SetCode(addr common.Address, code []byte) {
	acc := s.Get(addr).copy()
	acc.Code = append([]byte(nil), code...)
	s.set(addr, acc)
}

// Snapshot pushes a savepoint and returns its id. Every call frame takes one so
// a reverting sub-call can be undone without touching the caller's writes.
func (s *State) Snapshot() int {
	s.snapshots = append(s.snapshots, &snapshot{
		journalLen: len(s.journal),
		logCount:   len(s.logs),
		burned:     new(big.Int).Set(s.burnedOrZero()),
	})
	return len(s.snapshots) - 1
}

// RevertToSnapshot restores the world to a savepoint in place, undoing every
// write since and discarding that savepoint and any nested above it. Writes
// committed before the snapshot (e.g. gas already charged) stay, so a failed call
// is neither free nor replayable.
func (s *State) RevertToSnapshot(id int) {
	if id < 0 || id >= len(s.snapshots) {
		return
	}
	snap := s.snapshots[id]
	// Undo every write since the savepoint newest-first, restoring each journaled
	// prior value (or deleting an address that did not exist). Reverse order is
	// required when an address was written more than once.
	for i := len(s.journal) - 1; i >= snap.journalLen; i-- {
		e := s.journal[i]
		if e.existed {
			s.accounts[e.addr] = e.prev
		} else {
			delete(s.accounts, e.addr)
		}
		if e.wasTouched {
			s.touched[e.addr] = true
		} else {
			delete(s.touched, e.addr)
		}
	}
	s.journal = s.journal[:snap.journalLen]
	// Drop events emitted after the savepoint.
	s.logs = s.logs[:snap.logCount]
	// Restore the burn total: a burn in a reverted frame is undone too.
	s.burned = new(big.Int).Set(snap.burned)
	s.snapshots = s.snapshots[:id]
}

// DiscardSnapshots drops every savepoint without reverting, once a tx commits.
func (s *State) DiscardSnapshots() {
	s.snapshots = s.snapshots[:0]
	s.journal = s.journal[:0] // committed writes need no undo; free the log
}

// AddLog records an event (vm.StateDB). Logs are truncated on revert, so an
// event survives only if the emitting frame commits.
func (s *State) AddLog(log *common.Log) { s.logs = append(s.logs, log) }

// LogCount / LogsSince let ApplyTx carve out the logs one tx produced: record the
// count before running, read the tail after.
func (s *State) LogCount() int { return len(s.logs) }
func (s *State) LogsSince(start int) []*common.Log {
	if start >= len(s.logs) {
		return nil
	}
	return append([]*common.Log(nil), s.logs[start:]...)
}

// GetBalance / AddBalance / SubBalance are the balance half of vm.StateDB.
func (s *State) GetBalance(addr common.Address) *big.Int         { return s.Balance(addr) }
func (s *State) AddBalance(addr common.Address, amount *big.Int) { s.Credit(addr, amount) }
func (s *State) SubBalance(addr common.Address, amount *big.Int) {
	acc := s.Get(addr).copy()
	acc.Balance.Sub(acc.Balance, amount)
	s.set(addr, acc)
}

// SetNonce sets a nonce directly. Reconstruction-only, for rebuilding state from
// disk. The state transition must never call it: nonces advance by one in ApplyTx
// and nowhere else, or replay protection gains a second failure point.
func (s *State) SetNonce(addr common.Address, nonce uint64) {
	acc := s.Get(addr).copy()
	acc.Nonce = nonce
	s.set(addr, acc)
}

func (s *State) Credit(addr common.Address, amount *big.Int) {
	acc := s.Get(addr).copy()
	acc.Balance.Add(acc.Balance, amount)
	s.set(addr, acc)
}

// Root commits to the entire world state. Go randomises map iteration, so
// addresses must be sorted before hashing, or a node disagrees with itself
// between restarts.
func (s *State) Root() common.Hash {
	addrs := make([]common.Address, 0, len(s.accounts))
	for addr := range s.accounts {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return bytes.Compare(addrs[i][:], addrs[j][:]) < 0
	})
	leaves := make([][]byte, len(addrs))
	for i, addr := range addrs {
		e := common.NewEncoder()
		e.Raw(addr.Bytes())
		e.Bytes(s.accounts[addr].encode())
		leaves[i] = e.Done()
	}
	root := types.MerkleRoot(leaves)

	// Bind the burn total into the root so consensus enforces it. With nothing
	// burned the root is exactly the account merkle, so no pre-burn root shifts;
	// it diverges to keccak(accountsRoot ‖ burned) only once a burn has happened.
	if s.burnedOrZero().Sign() == 0 {
		return root
	}
	e := common.NewEncoder()
	e.Raw(root.Bytes())
	e.BigInt(s.burned)
	return common.Keccak256(e.Done())
}

// Burn records amount as permanently destroyed. The value must already have left
// an account; Burn only folds it into the consensus-tracked total bound into
// Root(). The one sanctioned break in value conservation.
func (s *State) Burn(amount *big.Int) {
	if amount == nil || amount.Sign() <= 0 {
		return
	}
	s.burned = new(big.Int).Add(s.burnedOrZero(), amount)
}

// Burned returns the cumulative destroyed supply (a copy).
func (s *State) Burned() *big.Int { return new(big.Int).Set(s.burnedOrZero()) }

// SetBurned restores the burn total when materialising state from disk. Must run
// before the head root is re-verified, since Root() commits to this value.
func (s *State) SetBurned(v *big.Int) {
	if v == nil {
		s.burned = new(big.Int)
		return
	}
	s.burned = new(big.Int).Set(v)
}

// Accounts returns a deep-copied snapshot, for RPC and debugging.
func (s *State) Accounts() map[common.Address]*Account {
	out := make(map[common.Address]*Account, len(s.accounts))
	for a, acc := range s.accounts {
		out[a] = acc.copy()
	}
	return out
}
