package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/state"
	"lxs/store"
	"lxs/types"
)

func openChain(t *testing.T, db store.KV, g *Genesis, retention uint64) *Blockchain {
	t.Helper()
	bc, err := NewBlockchain(db, g, Options{Retention: retention})
	if err != nil {
		t.Fatalf("opening chain: %v", err)
	}
	return bc
}

// Restart means discarding the Blockchain object and rebuilding it from the same
// database. Everything the node knew must come back: head, balances, nonces, tx
// index, receipts.
func TestRestartRestoresEverything(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	g := testGenesis(alice.Address())
	db := store.NewMemory()

	var txHashes []common.Hash
	var headHash common.Hash
	var headHeight uint64

	// --- first run ---
	{
		bc := openChain(t, db, g, 0)
		br := newBranch(t, bc, bc.Head())
		for i := 0; i < 5; i++ {
			tx := signedTxFor(t, alice, uint64(i), bob.Address(), 1000, int64(i+1))
			br.push(bc, tx)
			txHashes = append(txHashes, tx.Hash())
		}
		headHash = bc.Head().Hash()
		headHeight = bc.Head().Height()

		if got := bc.StateSnapshot().Balance(bob.Address()).Int64(); got != 5000 {
			t.Fatalf("bob before restart: %d", got)
		}
		// No Close(): a node must survive an unclean shutdown, not only a clean one.
	}

	// --- restart: new object, same database ---
	bc := openChain(t, db, g, 0)

	if bc.Head().Hash() != headHash {
		t.Fatalf("head after restart: got %s want %s", bc.Head().Hash().Hex(), headHash.Hex())
	}
	if bc.Head().Height() != headHeight {
		t.Fatalf("height after restart: got %d want %d", bc.Head().Height(), headHeight)
	}
	if got := bc.StateSnapshot().Balance(bob.Address()).Int64(); got != 5000 {
		t.Fatalf("bob after restart: got %d want 5000", got)
	}
	if got := bc.StateSnapshot().Nonce(alice.Address()); got != 5 {
		t.Fatalf("alice nonce after restart: got %d want 5", got)
	}
	// The state rebuilt from disk must hash to what the head block claimed.
	if bc.StateSnapshot().Root() != bc.Head().Header.StateRoot {
		t.Fatal("state root after restart does not match the head block header")
	}
	for i, h := range txHashes {
		if _, _, err := bc.TxByHash(h); err != nil {
			t.Fatalf("tx %d lost across restart: %v", i, err)
		}
		r, _, err := bc.ReceiptByTxHash(h)
		if err != nil {
			t.Fatalf("receipt %d lost across restart: %v", i, err)
		}
		if r.Status != types.ReceiptSuccess {
			t.Fatalf("receipt %d came back wrong", i)
		}
	}
	for h := uint64(0); h <= headHeight; h++ {
		if _, err := bc.BlockByHeight(h); err != nil {
			t.Fatalf("canonical block at height %d lost across restart", h)
		}
	}
}

// A node must keep producing after a restart, not start a parallel chain.
func TestChainContinuesAfterRestart(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	g := testGenesis(alice.Address())
	db := store.NewMemory()

	bc := openChain(t, db, g, 0)
	br := newBranch(t, bc, bc.Head())
	br.push(bc, signedTxFor(t, alice, 0, bob.Address(), 100, 1))
	br.push(bc, signedTxFor(t, alice, 1, bob.Address(), 100, 1))

	bc2 := openChain(t, db, g, 0)
	br2 := newBranch(t, bc2, bc2.Head())
	br2.push(bc2, signedTxFor(t, alice, 2, bob.Address(), 100, 1))

	if bc2.Head().Height() != 3 {
		t.Fatalf("height after restart + 1 block: got %d want 3", bc2.Head().Height())
	}
	if got := bc2.StateSnapshot().Balance(bob.Address()).Int64(); got != 300 {
		t.Fatalf("bob: got %d want 300", got)
	}
	if bc2.StateSnapshot().Nonce(alice.Address()) != 3 {
		t.Fatal("nonce did not continue across the restart")
	}
}

// Opening a database from a different chain must fail loudly; a node on the wrong
// network syncs nothing and reports nonsense.
func TestWrongChainDatabaseIsRefused(t *testing.T) {
	alice := newKey(t)
	db := store.NewMemory()

	g1 := testGenesis(alice.Address())
	bc := openChain(t, db, g1, 0)
	newBranch(t, bc, bc.Head()).grow(bc, 2)

	// Same chain id, different allocation => different genesis hash.
	g2 := testGenesis(newKey(t).Address())
	if _, err := NewBlockchain(db, g2, Options{}); err == nil {
		t.Fatal("a database from a different genesis was accepted")
	}

	// Different chain id entirely.
	g3 := testGenesis(alice.Address())
	g3.ChainID = 9999
	if _, err := NewBlockchain(db, g3, Options{}); err == nil {
		t.Fatal("a database from a different chain id was accepted")
	}
}

// In-memory states must be bounded by the retention window regardless of how long
// the chain runs.
func TestInMemoryStateIsBounded(t *testing.T) {
	alice := newKey(t)
	const retention = 8
	bc := openChain(t, store.NewMemory(), testGenesis(alice.Address()), retention)

	newBranch(t, bc, bc.Head()).grow(bc, 60)

	if bc.Head().Height() != 60 {
		t.Fatalf("height: got %d want 60", bc.Head().Height())
	}
	if n := bc.StateCount(); uint64(n) > retention+2 {
		t.Fatalf("in-memory states: got %d, retention is %d — the bound is not holding", n, retention)
	}
	// Blocks themselves are not pruned: they are the chain.
	for h := uint64(0); h <= 60; h++ {
		if _, err := bc.BlockByHeight(h); err != nil {
			t.Fatalf("block at height %d was pruned — blocks must survive: %v", h, err)
		}
	}
	// The tip's state is intact.
	if bc.StateSnapshot().Root() != bc.Head().Header.StateRoot {
		t.Fatal("head state root is wrong after pruning")
	}
}

// Pruning must not break reorgs inside the window.
func TestReorgStillWorksAfterPruning(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	const retention = 10
	bc := openChain(t, store.NewMemory(), testGenesis(alice.Address()), retention)

	// Run well past the retention window so pruning has happened.
	main := newBranch(t, bc, bc.Head()).grow(bc, 30)
	forkPoint := main[len(main)-4] // 4 blocks back, inside the window

	tx := signedTxFor(t, alice, 0, bob.Address(), 4242, 1)
	side := newBranch(t, bc, forkPoint)
	side.push(bc, tx)
	side.grow(bc, 5) // 27 + 1 + 5 = 33, taller than main's 30

	if bc.Head().Height() != 33 {
		t.Fatalf("head height: got %d want 33", bc.Head().Height())
	}
	if got := bc.StateSnapshot().Balance(bob.Address()).Int64(); got != 4242 {
		t.Fatalf("reorg after pruning lost the tx: bob has %d", got)
	}
	if bc.StateSnapshot().Root() != bc.Head().Header.StateRoot {
		t.Fatal("state root does not match head after a post-pruning reorg")
	}
}

// A reorg from before the retention window is refused, not silently mishandled:
// the finality assumption enforced rather than assumed.
func TestReorgDeeperThanRetentionIsRefused(t *testing.T) {
	alice := newKey(t)
	const retention = 5
	bc := openChain(t, store.NewMemory(), testGenesis(alice.Address()), retention)

	main := newBranch(t, bc, bc.Head()).grow(bc, 30)
	deep := main[2] // height 3, far outside the window

	// Building on it is impossible: its state is no longer held.
	if _, err := bc.StateAt(deep.Hash()); err == nil {
		t.Fatal("state outside the retention window is still materialised")
	}

	// The block itself is still there: it is the chain.
	if !bc.HasBlock(deep.Hash()) {
		t.Fatal("a canonical block was deleted")
	}
	headBefore := bc.Head().Hash()

	// A block claiming that ancient parent must be refused, not applied. Valid PoW,
	// so the refusal is about the retention window, not the proof.
	ts := deep.Header.Timestamp + TargetBlockTime
	forged := &types.Block{
		Header: &types.Header{
			ParentHash: deep.Hash(),
			Height:     deep.Height() + 1,
			Timestamp:  ts,
			GasLimit:   deep.Header.GasLimit,
			Proposer:   newKey(t).Address(),
			TxRoot:     types.TxRoot(nil),
			Difficulty: bc.RequiredDifficulty(deep.Header), // LWMA-derived, matches the validator
		},
	}
	mine(forged.Header, nil)
	if err := bc.InsertBlock(forged); err == nil {
		t.Fatal("a block forking from before the retention window was accepted")
	}
	if bc.Head().Hash() != headBefore {
		t.Fatal("a refused block moved the head")
	}
}

// Reverse diffs must restore an account that did not exist before the block to
// non-existent, not to a zero-balance record. A ghost account changes the state
// root; a storage bug that looks like a consensus bug.
func TestReorgRemovesAccountsThatShouldNotExist(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	db := store.NewMemory()
	g := testGenesis(alice.Address())
	bc := openChain(t, db, g, 0)
	genesis := bc.Head()

	rootBefore := bc.StateSnapshot().Root()

	// Branch A creates bob out of nothing.
	tx := signedTxFor(t, alice, 0, bob.Address(), 777, 1)
	newBranch(t, bc, genesis).push(bc, tx)
	if bc.StateSnapshot().Balance(bob.Address()).Int64() != 777 {
		t.Fatal("branch A did not credit bob")
	}

	// Branch B wins and never mentions bob.
	newBranch(t, bc, genesis).grow(bc, 2)

	if bc.StateSnapshot().Balance(bob.Address()).Sign() != 0 {
		t.Fatal("bob still has money after the reorg")
	}
	// The root must be recomputable and consistent, and the account table must not
	// contain a zeroed bob.
	if bc.StateSnapshot().Root() != bc.Head().Header.StateRoot {
		t.Fatal("state root after reorg does not match head")
	}
	if _, exists, err := loadAccount(db, bob.Address()); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("a ghost account survived the reorg on disk")
	}

	// A restart must agree.
	bc2 := openChain(t, db, g, 0)
	if bc2.StateSnapshot().Root() != bc2.Head().Header.StateRoot {
		t.Fatal("state root from disk disagrees with head after a reorg + restart")
	}
	_ = rootBefore
}

// A reorg followed by a restart must come back on the winning branch with its
// state. A disk out of step with the head pointer is a corruption that only shows
// up on the next boot.
func TestRestartAfterReorg(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	db := store.NewMemory()
	g := testGenesis(alice.Address())

	var winner common.Hash
	{
		bc := openChain(t, db, g, 0)
		genesis := bc.Head()

		newBranch(t, bc, genesis).push(bc, signedTxFor(t, alice, 0, bob.Address(), 111, 1))

		sb := newBranch(t, bc, genesis)
		sb.push(bc, signedTxFor(t, alice, 0, bob.Address(), 222, 1))
		sb.grow(bc, 2)
		winner = bc.Head().Hash()

		if bc.StateSnapshot().Balance(bob.Address()).Int64() != 222 {
			t.Fatal("wrong branch won")
		}
	}

	bc := openChain(t, db, g, 0)
	if bc.Head().Hash() != winner {
		t.Fatal("restart came back on the wrong branch")
	}
	if got := bc.StateSnapshot().Balance(bob.Address()).Int64(); got != 222 {
		t.Fatalf("bob after reorg + restart: got %d want 222", got)
	}
	if bc.StateSnapshot().Root() != bc.Head().Header.StateRoot {
		t.Fatal("state root from disk disagrees with head")
	}
}

// Every block insert must be one atomic batch. If a block's data could land
// without the head pointer (or vice versa), a crash mid-write corrupts the node.
func TestBlockInsertIsASingleAtomicBatch(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	db := &countingKV{Memory: store.NewMemory()}
	g := testGenesis(alice.Address())

	bc, err := NewBlockchain(db, g, Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.commits = 0

	newBranch(t, bc, bc.Head()).push(bc, signedTxFor(t, alice, 0, bob.Address(), 10, 1))

	if db.commits != 1 {
		t.Fatalf("inserting one block took %d commits — it must be exactly 1, or a crash between them corrupts the chain", db.commits)
	}
	if db.rawPuts != 0 {
		t.Fatalf("%d writes bypassed the batch — every one of them is a crash window", db.rawPuts)
	}
}

// The integrity check that makes JSON storage safe: a block that does not hash to
// the key it is filed under must be refused, not loaded.
func TestCorruptBlockIsDetectedNotLoaded(t *testing.T) {
	alice := newKey(t)
	db := store.NewMemory()
	g := testGenesis(alice.Address())
	bc := openChain(t, db, g, 0)

	blk := newBranch(t, bc, bc.Head()).push(bc)
	h := blk.Hash()

	// Tamper with the stored bytes, as a bad disk or a bad round-trip would.
	raw, err := db.Get(blockKey(h))
	if err != nil {
		t.Fatal(err)
	}
	tampered := make([]byte, len(raw))
	copy(tampered, raw)
	// Flip a hashed field in the JSON; any change will do.
	for i := 0; i+8 < len(tampered); i++ {
		if string(tampered[i:i+8]) == `"height"` {
			tampered[i+2] = 'x'
			break
		}
	}
	db.Put(blockKey(h), tampered)

	if _, err := loadBlock(db, h); err == nil {
		t.Fatal("a corrupt block was loaded — the hash check is not doing its job")
	}
}

func TestSupplyInvariantAcrossRestart(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	db := store.NewMemory()
	g := testGenesis(alice.Address(), bob.Address())

	var want *big.Int
	{
		bc := openChain(t, db, g, 0)
		base := supplyOf(bc) // genesis
		br := newBranch(t, bc, bc.Head())
		for i := 0; i < 10; i++ {
			br.push(bc, signedTxFor(t, alice, uint64(i), bob.Address(), 500, 7))
		}
		// 10 canonical blocks issued 10 rewards; that issued supply must survive the
		// restart, with no coin lost or doubled by reloading from disk.
		want = new(big.Int).Add(base, new(big.Int).Mul(big.NewInt(10), state.BaseBlockReward))
		if got := supplyOf(bc); got.Cmp(want) != 0 {
			t.Fatalf("issuance before restart: got %s want %s", got, want)
		}
	}

	bc := openChain(t, db, g, 0)
	if got := supplyOf(bc); got.Cmp(want) != 0 {
		t.Fatalf("supply changed across the restart: got %s want %s", got, want)
	}
}

func supplyOf(bc *Blockchain) *big.Int {
	// Total issued supply = circulating (balances) + destroyed (burned). The burned
	// total is added back to measure issuance and prove it survives a restart.
	sum := new(big.Int).Set(bc.StateSnapshot().Burned())
	for _, acc := range bc.StateSnapshot().Accounts() {
		sum.Add(sum, acc.Balance)
	}
	return sum
}

// countingKV counts commits and un-batched writes, so a test can assert a block
// insert is one atomic operation.
type countingKV struct {
	*store.Memory
	commits int
	rawPuts int
}

func (c *countingKV) Put(key, value []byte) error {
	c.rawPuts++
	return c.Memory.Put(key, value)
}

func (c *countingKV) NewBatch() store.Batch {
	return &countingBatch{Batch: c.Memory.NewBatch(), owner: c}
}

type countingBatch struct {
	store.Batch
	owner *countingKV
}

func (b *countingBatch) Commit() error {
	b.owner.commits++
	return b.Batch.Commit()
}
