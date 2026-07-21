package core

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"

	"lxs/common"
	"lxs/state"
	"lxs/store"
	"lxs/types"
)

// On-disk schema. Each table gets a one-byte, disjoint prefix, so a prefix scan
// for one table cannot match another.
const (
	prefixBlock     = 'b' // 'b' + blockHash        -> block JSON
	prefixCanonical = 'c' // 'c' + height(8, BE)    -> canonical block hash
	prefixReceipts  = 'r' // 'r' + blockHash        -> receipts JSON
	prefixTxIndex   = 't' // 't' + txHash           -> TxLocation JSON
	prefixAccount   = 'a' // 'a' + address          -> account JSON (canonical state)
	prefixRevDiff   = 'd' // 'd' + blockHash        -> reverse diff JSON
	prefixTD        = 'w' // 'w' + blockHash        -> total difficulty (big-endian) — 'w' for work
)

// Singleton keys.
var (
	keyHead       = []byte("!head")    // canonical head block hash
	keyGenesis    = []byte("!genesis") // genesis hash; guards against opening the wrong chain
	keySchema     = []byte("!schema")  // schema version
	keyChainID    = []byte("!chainid")
	keyBurned     = []byte("!burned") // cumulative destroyed supply at the canonical head
	schemaVersion = uint64(1)
)

// Heights are big-endian so byte order equals numeric order; little-endian would
// order height 256 before height 2 under a prefix scan.
func canonicalKey(height uint64) []byte {
	k := make([]byte, 1+8)
	k[0] = prefixCanonical
	binary.BigEndian.PutUint64(k[1:], height)
	return k
}

func blockKey(h common.Hash) []byte    { return append([]byte{prefixBlock}, h[:]...) }
func receiptsKey(h common.Hash) []byte { return append([]byte{prefixReceipts}, h[:]...) }
func txIndexKey(h common.Hash) []byte  { return append([]byte{prefixTxIndex}, h[:]...) }
func accountKey(a common.Address) []byte {
	return append([]byte{prefixAccount}, a[:]...)
}
func revDiffKey(h common.Hash) []byte { return append([]byte{prefixRevDiff}, h[:]...) }
func tdKey(h common.Hash) []byte      { return append([]byte{prefixTD}, h[:]...) }

// putTD records a block's total difficulty (accumulated work) as big-endian bytes,
// so the fork choice can weigh chains without re-walking to genesis.
func putTD(b store.Batch, h common.Hash, td *big.Int) { b.Put(tdKey(h), td.Bytes()) }

func getTD(db store.KV, h common.Hash) (*big.Int, error) {
	data, err := db.Get(tdKey(h))
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(data), nil
}

// Storage uses JSON; hashing uses the canonical binary encoder in common/. Hashing
// needs a byte-exact, cross-implementation encoding, which JSON is not (key order
// is unspecified). loadBlock re-derives the block hash and refuses a mismatch, so
// a mangled round-trip cannot run. *big.Int survives JSON Go-to-Go only; other
// readers lose precision, which is why the RPC layer uses hex strings.

type storedDiffEntry struct {
	Address common.Address `json:"address"`
	// Existed distinguishes "no account before the block" from "zero balance".
	// Restoring the wrong one leaves a zero-balance ghost, so the disk root stops
	// matching the in-memory root.
	Existed bool                        `json:"existed"`
	Nonce   uint64                      `json:"nonce"`
	Balance *big.Int                    `json:"balance"`
	Storage map[common.Hash]common.Hash `json:"storage,omitempty"`
	// Code is folded into the account hash, so unwinding a block that touched a
	// pre-existing contract must restore it — otherwise the reorg leaves the
	// contract codeless on disk and the next restart's root check fails.
	Code []byte `json:"code,omitempty"`
}

// storedDiff is the reverse diff of a block: every touched account as it was
// before the block ran. Reverse because a reorg unwinds by restoring overwritten
// values; a forward diff would require re-executing from the fork point.
type storedDiff struct {
	Entries []storedDiffEntry `json:"entries"`
	// BurnDelta is how much this block added to the cumulative burn total. The
	// account entries restore balances/nonces/storage/code on unwind, but the burn
	// scalar folds into the state root separately, so its per-block change must be
	// recorded too — otherwise a state rebuilt by replaying reverse diffs (restart
	// window rebuild) roots differently and cannot be trusted for a reorg.
	BurnDelta *big.Int `json:"burnDelta,omitempty"`
}

func encodeJSON(v interface{}) ([]byte, error) { return json.Marshal(v) }

func decodeJSON(data []byte, v interface{}) error { return json.Unmarshal(data, v) }

// writeBlock queues a block and its receipts. Content-addressed by hash: written
// once, never updated, since a block is immutable.
func writeBlock(b store.Batch, blk *types.Block, receipts []*types.Receipt) error {
	data, err := encodeJSON(blk)
	if err != nil {
		return err
	}
	b.Put(blockKey(blk.Hash()), data)

	rdata, err := encodeJSON(receipts)
	if err != nil {
		return err
	}
	b.Put(receiptsKey(blk.Hash()), rdata)
	return nil
}

func loadBlock(db store.KV, h common.Hash) (*types.Block, error) {
	data, err := db.Get(blockKey(h))
	if err != nil {
		return nil, err
	}
	var blk types.Block
	if err := decodeJSON(data, &blk); err != nil {
		return nil, fmt.Errorf("core: decoding block %s: %w", h.Hex(), err)
	}
	// Integrity check that makes JSON storage safe: if the decoded struct does not
	// hash to the key it was filed under, the store is corrupt.
	if got := blk.Hash(); got != h {
		return nil, fmt.Errorf("core: block %s decodes to %s — store is corrupt", h.Hex(), got.Hex())
	}
	return &blk, nil
}

func loadReceipts(db store.KV, h common.Hash) ([]*types.Receipt, error) {
	data, err := db.Get(receiptsKey(h))
	if err != nil {
		return nil, err
	}
	var rs []*types.Receipt
	if err := decodeJSON(data, &rs); err != nil {
		return nil, err
	}
	return rs, nil
}

// writeAccountChanges queues the state diff for a block plus the reverse diff to
// undo it. Must be called before the batch commits, since it reads previous values
// from the current database and the batch is not yet visible.
func writeAccountChanges(b store.Batch, blk *types.Block, next, prev *state.State, touched []common.Address) error {
	diff := storedDiff{
		Entries:   make([]storedDiffEntry, 0, len(touched)),
		BurnDelta: new(big.Int).Sub(next.Burned(), prev.Burned()), // burn added by this block
	}

	for _, addr := range touched {
		// The reverse-diff base is the account's value at the parent, read from the
		// parent state, never from disk. During a multi-block reorg the added blocks
		// share one uncommitted batch, so disk holds a stale pre-reorg value; the
		// parent state is exactly the world before the block
		// (TestReorgStatePersistsAcrossRestart).
		entry := storedDiffEntry{Address: addr, Existed: prev.Exists(addr)}
		if entry.Existed {
			p := prev.Get(addr)
			entry.Nonce = p.Nonce
			entry.Balance = p.Balance
			entry.Storage = p.Storage // undo of any SSTORE the block did
			entry.Code = p.Code       // undo for a block that touched a contract
		}
		diff.Entries = append(diff.Entries, entry)

		// New value.
		if !next.Exists(addr) {
			// Emptied accounts are deleted, not stored as zeroes, matching state.set
			// in memory; a zero record on disk would add a leaf the in-memory root
			// lacks.
			b.Delete(accountKey(addr))
			continue
		}
		acc := next.Get(addr)
		data, err := encodeJSON(acc)
		if err != nil {
			return err
		}
		b.Put(accountKey(addr), data)
	}

	ddata, err := encodeJSON(diff)
	if err != nil {
		return err
	}
	b.Put(revDiffKey(blk.Hash()), ddata)
	return nil
}

func loadAccount(db store.KV, addr common.Address) (*state.Account, bool, error) {
	data, err := db.Get(accountKey(addr))
	if err == store.ErrNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var acc state.Account
	if err := decodeJSON(data, &acc); err != nil {
		return nil, false, err
	}
	return &acc, true, nil
}

// loadStoredDiff reads a block's reverse diff as a struct (applyReverseDiff replays it
// into a batch; the restart window-rebuild needs it in memory).
func loadStoredDiff(db store.KV, blockHash common.Hash) (*storedDiff, error) {
	data, err := db.Get(revDiffKey(blockHash))
	if err != nil {
		return nil, err
	}
	var diff storedDiff
	if err := decodeJSON(data, &diff); err != nil {
		return nil, err
	}
	return &diff, nil
}

// applyReverseDiff queues the undo of a block's account changes.
func applyReverseDiff(db store.KV, b store.Batch, blockHash common.Hash) error {
	data, err := db.Get(revDiffKey(blockHash))
	if err != nil {
		return fmt.Errorf("core: no reverse diff for %s: %w", blockHash.Hex(), err)
	}
	var diff storedDiff
	if err := decodeJSON(data, &diff); err != nil {
		return err
	}
	for _, e := range diff.Entries {
		if !e.Existed {
			b.Delete(accountKey(e.Address))
			continue
		}
		enc, err := encodeJSON(&state.Account{Nonce: e.Nonce, Balance: e.Balance, Storage: e.Storage, Code: e.Code})
		if err != nil {
			return err
		}
		b.Put(accountKey(e.Address), enc)
	}
	return nil
}

// loadState materialises the full canonical account table. O(accounts), like
// State.Root(); runs once at startup, not per block. Pending the Merkle Patricia
// Trie's structural sharing.
func loadState(db store.KV) (*state.State, error) {
	s := state.New()
	it := db.Iterate([]byte{prefixAccount})
	defer it.Close()

	for it.Next() {
		key := it.Key()
		if len(key) != 1+common.AddressLength {
			return nil, fmt.Errorf("core: malformed account key of length %d", len(key))
		}
		var addr common.Address
		copy(addr[:], key[1:])

		var acc state.Account
		if err := decodeJSON(it.Value(), &acc); err != nil {
			return nil, err
		}
		// Restore the whole account in ONE shot. The slot-by-slot alternative
		// (SetStorage per key) deep-copies the growing storage map on every call —
		// O(slots²) time and garbage per contract, so a single widely-held launchpad
		// token (one storage slot per holder) would make every restart take minutes or
		// OOM, exactly when adoption succeeds. Code is included: it folds into the
		// account hash, so dropping it would diverge the rebuilt root and brick startup.
		s.RestoreAccount(addr, acc.Nonce, acc.Balance, acc.Storage, acc.Code)
	}
	if err := it.Error(); err != nil {
		return nil, err
	}
	s.ClearTouched()
	return s, nil
}

func putUint64(b store.Batch, key []byte, v uint64) {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	b.Put(key, buf)
}

func getUint64(db store.KV, key []byte) (uint64, error) {
	data, err := db.Get(key)
	if err != nil {
		return 0, err
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("core: malformed uint64 at %s", key)
	}
	return binary.BigEndian.Uint64(data), nil
}

// putBigInt / getBigInt store a non-negative big.Int as its big-endian magnitude.
// Used for the burn total, written as the absolute value at the canonical head on
// every commit (no reverse diff), so a reorg overwrites it with the new head's
// total and avoids the staleness trap accounts hit.
func putBigInt(b store.Batch, key []byte, v *big.Int) {
	if v == nil {
		v = new(big.Int)
	}
	b.Put(key, v.Bytes())
}

func getBigInt(db store.KV, key []byte) (*big.Int, error) {
	data, err := db.Get(key)
	if err == store.ErrNotFound {
		return new(big.Int), nil // absent = nothing burned yet
	}
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(data), nil
}

func putHash(b store.Batch, key []byte, h common.Hash) { b.Put(key, h[:]) }

func getHash(db store.KV, key []byte) (common.Hash, error) {
	data, err := db.Get(key)
	if err != nil {
		return common.ZeroHash, err
	}
	return common.HashFromBytes(data)
}
