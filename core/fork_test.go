package core

import (
	"math/big"
	"math/rand"
	"testing"

	"lxs/common"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

// branch is a test-side chain builder. It tracks its own state, so it can build on
// blocks not yet inserted into the chain, unlike the real Producer which only
// builds on head.
type branch struct {
	t        *testing.T
	tip      *types.Block
	st       *state.State
	proposer common.Address
}

func newBranch(t *testing.T, bc *Blockchain, parent *types.Block) *branch {
	t.Helper()
	st, err := bc.StateAt(parent.Hash())
	if err != nil {
		t.Fatalf("no state at parent %s: %v", parent.Hash().Hex(), err)
	}
	return &branch{t: t, tip: parent, st: st, proposer: newKey(t).Address()}
}

// next builds the next block on this branch without inserting it.
func (b *branch) next(txs ...*types.Transaction) *types.Block {
	b.t.Helper()

	ts := b.tip.Header.Timestamp + 1000
	header := &types.Header{
		ParentHash: b.tip.Hash(),
		Height:     b.tip.Height() + 1,
		Timestamp:  ts,
		GasLimit:   b.tip.Header.GasLimit,
		Proposer:   b.proposer,
		Difficulty: b.tip.Header.Difficulty, // < LwmaWindow blocks: difficulty holds at genesis
	}

	var gasUsed uint64
	included := make([]*types.Transaction, 0, len(txs))
	receipts := make([]*types.Receipt, 0, len(txs))
	// Match the real producer/validator: the VM's block environment is this
	// header's, so a tx using NUMBER/TIMESTAMP roots identically.
	b.st.SetBlockContext(header.Height, uint64(header.Timestamp)/1000, header.Difficulty)
	for _, tx := range txs {
		used, status, fkLogs, err := state.ApplyTx(b.st, tx, b.proposer, header.GasLimit)
		if err != nil {
			b.t.Fatalf("test block contains an invalid tx: %v", err)
		}
		gasUsed += used
		included = append(included, tx)
		receipts = append(receipts, &types.Receipt{
			Status: status, GasUsed: used, CumulativeGasUsed: gasUsed, Logs: fkLogs,
		})
	}

	// Same block reward ApplyBlock applies on validation, or the state root
	// will not match.
	state.CreditBlockReward(b.st, b.proposer, header.Height)

	header.TxRoot = types.TxRoot(included)
	header.ReceiptRoot = types.ReceiptRoot(receipts)
	header.StateRoot = b.st.Root()
	header.GasUsed = gasUsed
	// Trivial at difficulty 1 (nonce 0 wins), but goes through the real miner so
	// the test blocks carry a genuine proof.
	mine(header, nil)

	blk := &types.Block{Header: header, Txs: included}
	b.tip = blk
	return blk
}

// push builds a block and inserts it.
func (b *branch) push(bc *Blockchain, txs ...*types.Transaction) *types.Block {
	b.t.Helper()
	blk := b.next(txs...)
	if err := bc.InsertBlock(blk); err != nil && err != ErrKnownBlock {
		b.t.Fatalf("inserting block %d: %v", blk.Height(), err)
	}
	return blk
}

// grow builds and inserts n empty blocks.
func (b *branch) grow(bc *Blockchain, n int) []*types.Block {
	b.t.Helper()
	out := make([]*types.Block, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, b.push(bc))
	}
	return out
}

func TestSideChainIsStoredButNotCanonical(t *testing.T) {
	alice := newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	// Two competing blocks at height 1 from different proposers, so they hash
	// differently.
	a := newBranch(t, bc, genesis).next()
	b := newBranch(t, bc, genesis).next()

	if err := bc.InsertBlock(a); err != nil {
		t.Fatal(err)
	}
	if err := bc.InsertBlock(b); err != nil {
		t.Fatal(err)
	}

	// Both are known and both have state.
	if !bc.HasBlock(a.Hash()) || !bc.HasBlock(b.Hash()) {
		t.Fatal("a valid block was not stored")
	}
	if _, err := bc.StateAt(a.Hash()); err != nil {
		t.Fatal("no state for side chain block")
	}

	// Exactly one is canonical.
	if bc.IsCanonical(a.Hash()) == bc.IsCanonical(b.Hash()) {
		t.Fatal("exactly one of two competing tips must be canonical")
	}
	if bc.Head().Height() != 1 {
		t.Fatalf("head height: got %d want 1", bc.Head().Height())
	}
}

func TestTallerChainWins(t *testing.T) {
	alice := newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	short := newBranch(t, bc, genesis).grow(bc, 2)
	if bc.Head().Hash() != short[1].Hash() {
		t.Fatal("head did not follow the first chain")
	}

	// A competing branch from genesis, one block taller.
	long := newBranch(t, bc, genesis).grow(bc, 3)

	if bc.Head().Hash() != long[2].Hash() {
		t.Fatal("the taller chain did not become head")
	}
	if bc.Head().Height() != 3 {
		t.Fatalf("head height: got %d want 3", bc.Head().Height())
	}
	for _, blk := range short {
		if bc.IsCanonical(blk.Hash()) {
			t.Fatal("an orphaned block is still marked canonical")
		}
		if !bc.HasBlock(blk.Hash()) {
			t.Fatal("an orphaned block was deleted — it may win again later")
		}
	}
}

// A tx mined on a branch that gets orphaned must stop being reported as mined.
// If txIndex is not unwound, the node reports a tx as confirmed while it is back
// in the mempool and may never be mined again.
func TestTxIndexIsUnwoundOnReorg(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	tx := signedTxFor(t, alice, 0, bob.Address(), 5000, 1)

	// Branch A mines the tx.
	a1 := newBranch(t, bc, genesis).push(bc, tx)
	if _, _, err := bc.TxByHash(tx.Hash()); err != nil {
		t.Fatal("tx should be indexed after being mined")
	}
	if _, _, err := bc.ReceiptByTxHash(tx.Hash()); err != nil {
		t.Fatal("receipt should exist after being mined")
	}

	// Branch B is taller and does not contain the tx.
	newBranch(t, bc, genesis).grow(bc, 2)

	if bc.IsCanonical(a1.Hash()) {
		t.Fatal("orphaned block is still canonical")
	}
	if _, _, err := bc.TxByHash(tx.Hash()); err == nil {
		t.Fatal("orphaned tx is STILL reported as mined — the node is lying")
	}
	if _, _, err := bc.ReceiptByTxHash(tx.Hash()); err == nil {
		t.Fatal("orphaned tx still has a receipt — the node is lying")
	}
	// The money must have moved back.
	if bc.StateSnapshot().Balance(bob.Address()).Sign() != 0 {
		t.Fatal("bob still holds money from an orphaned transaction")
	}
}

// A reorg must not destroy transactions. They left the pool when mined; if nobody
// takes them back when the block is orphaned, the sender's money never moves.
func TestOrphanedTxsGoBackToTheMempool(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1000)
	BindMempool(bc, pool)
	genesis := bc.Head()

	tx := signedTxFor(t, alice, 0, bob.Address(), 5000, 1)
	if err := pool.Add(tx, testChainID); err != nil {
		t.Fatal(err)
	}

	// Branch A mines it; the producer would normally evict it from the pool.
	a1 := newBranch(t, bc, genesis).push(bc, tx)
	pool.Remove(a1.Txs)
	if pool.Len() != 0 {
		t.Fatal("mined tx should have left the pool")
	}

	// Branch B wins and does not contain the tx.
	newBranch(t, bc, genesis).grow(bc, 2)

	if pool.Len() != 1 {
		t.Fatalf("orphaned tx was destroyed by the reorg: pool has %d txs", pool.Len())
	}
	if _, ok := pool.Get(tx.Hash()); !ok {
		t.Fatal("the wrong tx came back")
	}
}

// The ordering trap documented in BindMempool. A tx present on both branches must
// end up removed, not re-injected; the wrong order drains nothing, as the producer
// keeps re-mining a tx whose nonce is already spent.
func TestTxOnBothBranchesEndsUpRemoved(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	pool := mempool.New(1000)
	BindMempool(bc, pool)
	genesis := bc.Head()

	shared := signedTxFor(t, alice, 0, bob.Address(), 1000, 1)

	// Branch A: height 1, contains the tx.
	a1 := newBranch(t, bc, genesis).push(bc, shared)
	pool.Remove(a1.Txs)

	// Branch B: height 1 contains the same tx, then height 2 makes it win.
	bb := newBranch(t, bc, genesis)
	b1 := bb.push(bc, shared)
	b2 := bb.push(bc)

	if bc.Head().Hash() != b2.Hash() {
		t.Fatal("branch B should have won")
	}
	if pool.Len() != 0 {
		t.Fatalf("a tx that is still mined was re-injected: pool has %d", pool.Len())
	}
	// And it must still be reported as mined, on its new block.
	_, loc, err := bc.TxByHash(shared.Hash())
	if err != nil {
		t.Fatal("tx should still be mined on the new canonical chain")
	}
	if loc.BlockHash != b1.Hash() {
		t.Fatal("tx index points at the orphaned block, not the canonical one")
	}
}

// Two nodes receiving the same blocks in different orders must end up on the same
// head, or the network partitions silently. A property test because the failure
// mode is order-dependent, so one order proves nothing.
func TestConvergenceUnderArbitraryArrivalOrder(t *testing.T) {
	alice := newKey(t)
	g := testGenesis(alice.Address())

	// Build a branchy tree once, on a scratch chain.
	scratch := NewMemBlockchain(g)
	genesis := scratch.Head()
	var all []*types.Block

	trunk := newBranch(t, scratch, genesis).grow(scratch, 3)
	all = append(all, trunk...)
	// A fork from height 1, four long: this one should win.
	fork := newBranch(t, scratch, trunk[0]).grow(scratch, 4)
	all = append(all, fork...)
	// A short fork from genesis that should lose.
	stub := newBranch(t, scratch, genesis).grow(scratch, 1)
	all = append(all, stub...)

	expected := scratch.Head().Hash()

	for trial := 0; trial < 200; trial++ {
		rng := rand.New(rand.NewSource(int64(trial)))
		shuffled := make([]*types.Block, len(all))
		copy(shuffled, all)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})

		bc := NewMemBlockchain(g)

		// Blocks whose parent has not arrived yet are deferred, as a real node
		// would queue rather than discard them.
		pending := shuffled
		for len(pending) > 0 {
			var deferred []*types.Block
			progress := false
			for _, blk := range pending {
				err := bc.InsertBlock(blk)
				switch {
				case err == nil, err == ErrKnownBlock:
					progress = true
				default:
					deferred = append(deferred, blk)
				}
			}
			if !progress {
				t.Fatalf("trial %d: stuck with %d blocks unapplied", trial, len(deferred))
			}
			pending = deferred
		}

		if bc.Head().Hash() != expected {
			t.Fatalf("trial %d: arrival order changed the head\n got  %s\n want %s",
				trial, bc.Head().Hash().Hex(), expected.Hex())
		}
	}
}

// The fork choice must be total: it must never call two distinct tips equal, or
// nodes seeing them in opposite orders never reconcile.
func TestForkChoiceIsTotalAndAntisymmetric(t *testing.T) {
	alice := newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	fc := HeaviestChain{}
	a := newBranch(t, bc, genesis).next()
	b := newBranch(t, bc, genesis).next()

	// Two siblings at the same height carry equal work, so total ordering rests
	// entirely on the hash tie-break.
	ta := &Tip{a.Header, big.NewInt(2)}
	tb := &Tip{b.Header, big.NewInt(2)}

	ab := fc.Better(ta, tb)
	ba := fc.Better(tb, ta)
	if ab == ba {
		t.Fatal("fork choice is not total: two distinct tips compare equal")
	}
	// And stable across repeated calls.
	for i := 0; i < 20; i++ {
		if fc.Better(ta, tb) != ab {
			t.Fatal("fork choice is not deterministic")
		}
	}
	// A block is never better than itself.
	if fc.Better(ta, ta) {
		t.Fatal("a block should not replace itself")
	}
}

func TestUnknownParentIsRejected(t *testing.T) {
	alice := newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	br := newBranch(t, bc, genesis)
	br.next() // built but never inserted
	child := br.next()

	err := bc.InsertBlock(child)
	if err == nil {
		t.Fatal("a block with an unknown parent was accepted")
	}
	if !isUnknownParent(err) {
		t.Fatalf("wrong error: %v", err)
	}
	if bc.HasBlock(child.Hash()) {
		t.Fatal("an unvalidated orphan was stored")
	}
}

// An invalid block on a side chain must not corrupt the canonical chain and must
// not be remembered.
func TestInvalidSideChainBlockDoesNotCorruptCanonical(t *testing.T) {
	alice := newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	good := newBranch(t, bc, genesis).grow(bc, 2)

	// A taller side chain that would win the fork choice, but its tip lies about
	// the state root.
	ev := newBranch(t, bc, genesis)
	ev.push(bc)
	ev.push(bc)
	evil3 := ev.next()
	evil3.Header.StateRoot = common.Hash{0xba, 0xad}

	if err := bc.InsertBlock(evil3); err == nil {
		t.Fatal("a block with a forged state root was accepted")
	}
	if bc.HasBlock(evil3.Hash()) {
		t.Fatal("an invalid block was stored in the tree")
	}

	// evil2 was valid and taller than good, so the head legitimately moved there.
	// The forged block must not win.
	if bc.Head().Hash() == evil3.Hash() {
		t.Fatal("head is a forged block")
	}
	_ = good
}

func TestDeepReorg(t *testing.T) {
	alice := newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	short := newBranch(t, bc, genesis).grow(bc, 10)
	if bc.Head().Hash() != short[9].Hash() {
		t.Fatal("head is wrong before the reorg")
	}

	long := newBranch(t, bc, genesis).grow(bc, 15)

	if bc.Head().Hash() != long[14].Hash() {
		t.Fatal("deep reorg did not take")
	}
	// Every height on the canonical chain must resolve to the new branch.
	for i, blk := range long {
		got, err := bc.BlockByHeight(uint64(i + 1))
		if err != nil {
			t.Fatalf("height %d missing after reorg", i+1)
		}
		if got.Hash() != blk.Hash() {
			t.Fatalf("height %d points at the wrong block after reorg", i+1)
		}
	}
	// And no orphaned height lingers.
	if _, err := bc.BlockByHeight(16); err == nil {
		t.Fatal("a height beyond head is still resolvable")
	}
}

// Supply must survive a reorg. The reorg rewinds real balance changes; if unwinding
// is not a clean state swap, money appears or vanishes.
func TestSupplyInvariantAcrossReorg(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address(), bob.Address()))
	genesis := bc.Head()

	supply := func() *big.Int {
		// Total issued supply = circulating (balances) + destroyed (burned). Fee
		// burn means balances alone fall short of issuance by exactly what was
		// burned; adding it back isolates issuance.
		sum := new(big.Int).Set(bc.StateSnapshot().Burned())
		for _, acc := range bc.StateSnapshot().Accounts() {
			sum.Add(sum, acc.Balance)
		}
		return sum
	}
	base := supply() // genesis supply, before any issuance
	reward := state.BaseBlockReward

	// Each canonical block issues exactly one reward, so canonical supply =
	// genesis + height * reward. A reorg must unwind the orphaned branch's issuance
	// as cleanly as its transfers.
	tx := signedTxFor(t, alice, 0, bob.Address(), 50_000, 5)
	newBranch(t, bc, genesis).push(bc, tx) // branch A: height 1
	wantA := new(big.Int).Add(base, reward)
	if supply().Cmp(wantA) != 0 {
		t.Fatalf("branch A issuance: got %s want %s", supply(), wantA)
	}

	other := signedTxFor(t, alice, 0, bob.Address(), 999, 2)
	bb := newBranch(t, bc, genesis)
	bb.push(bc, other)
	b2 := bb.push(bc) // branch B: height 2, wins

	if bc.Head().Hash() != b2.Hash() {
		t.Fatal("branch B should have won")
	}
	// Head is now height 2, so exactly two rewards: branch A's issuance was unwound,
	// not double-counted on top of B's.
	wantB := new(big.Int).Add(base, new(big.Int).Mul(big.NewInt(2), reward))
	if supply().Cmp(wantB) != 0 {
		t.Fatalf("supply after reorg: got %s want %s (genesis + 2 rewards)", supply(), wantB)
	}
}

// A reorg must restore the exact state of the branch it moves to.
func TestStateAfterReorgMatchesBranchState(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	txA := signedTxFor(t, alice, 0, bob.Address(), 111, 1)
	newBranch(t, bc, genesis).push(bc, txA)

	txB := signedTxFor(t, alice, 0, bob.Address(), 222, 1)
	bb := newBranch(t, bc, genesis)
	bb.push(bc, txB)
	b2 := bb.push(bc)

	if bc.Head().Hash() != b2.Hash() {
		t.Fatal("branch B should have won")
	}
	if got := bc.StateSnapshot().Balance(bob.Address()).Int64(); got != 222 {
		t.Fatalf("bob balance after reorg: got %d want 222 (branch B's tx)", got)
	}
	if bc.StateSnapshot().Root() != b2.Header.StateRoot {
		t.Fatal("head state root does not match the head block's header")
	}
}

func TestDuplicateInsertIsRejectedNotReapplied(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	tx := signedTxFor(t, alice, 0, bob.Address(), 400, 1)
	blk := newBranch(t, bc, genesis).push(bc, tx)
	before := bc.StateSnapshot().Balance(bob.Address()).Int64()

	// Gossip delivers the same block twice; applying it again would double-spend.
	if err := bc.InsertBlock(blk); err != ErrKnownBlock {
		t.Fatalf("duplicate insert: got %v want ErrKnownBlock", err)
	}
	if got := bc.StateSnapshot().Balance(bob.Address()).Int64(); got != before {
		t.Fatalf("a duplicate block was applied twice: %d -> %d", before, got)
	}
}

func signedTxFor(t *testing.T, k *crypto.PrivateKey, nonce uint64, to common.Address, value, gasPrice int64) *types.Transaction {
	t.Helper()
	tx := types.NewTransaction(testChainID, nonce, to, big.NewInt(value), types.IntrinsicGas, big.NewInt(gasPrice), nil)
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

func isUnknownParent(err error) bool {
	for err != nil {
		if err == ErrUnknownParent {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
