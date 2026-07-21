package core

import (
	"errors"
	"math/big"
	"math/rand"
	"testing"

	"lxs/common"
	"lxs/crypto"
	"lxs/store"
	"lxs/types"
)

// reorgStress builds a random tree of tx-carrying blocks on bc, forcing many
// reorgs, and returns the blocks it built. Shared by the in-memory residue test
// and the restart test.
func reorgStress(t *testing.T, bc *Blockchain, keys []*crypto.PrivateKey, addrs []common.Address, r *rand.Rand, rounds int) {
	t.Helper()
	blocks := []*types.Block{bc.Head()}
	used := 0
	for round := 0; round < rounds; round++ {
		parent := blocks[r.Intn(len(blocks))]
		br := newBranch(t, bc, parent)
		var txs []*types.Transaction
		if used < len(keys) {
			txs = append(txs, xfer(t, keys[used], 0, addrs[r.Intn(len(addrs))], int64(1+r.Intn(1000))))
			used++
		}
		blk := br.next(txs...)
		if err := bc.InsertBlock(blk); err != nil && err != ErrKnownBlock {
			t.Fatalf("insert h%d: %v", blk.Height(), err)
		}
		blocks = append(blocks, blk)
	}
}

// Stress the reorg machinery. A reorg must unwind the abandoned branch's
// transactions (balances, nonces) and its indexes (a non-canonical tx must stop
// being findable), then reapply the winning branch. A wrong unwind leaves residue
// from a branch that never happened: phantom balances a double-spend builds on.

func xfer(t *testing.T, k *crypto.PrivateKey, nonce uint64, to common.Address, value int64) *types.Transaction {
	t.Helper()
	tx := types.NewTransaction(testChainID, nonce, to, big.NewInt(value), types.IntrinsicGas, big.NewInt(1), nil)
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

// Branch A takes hold and moves coins to bobA, then a heavier branch B reorgs it
// out. Afterwards bobA's coins must be gone (A's tx unwound), bobB's present, the
// sender's nonce must reflect B only, and A's tx must no longer be indexed.
func TestDeepReorgUnwindsTxEffects(t *testing.T) {
	alice := newKey(t)
	bobA := common.Address{0xA0}
	bobB := common.Address{0xB0}

	bc := NewMemBlockchain(testGenesis(alice.Address()))
	genesis := bc.Head()

	insert := func(blk *types.Block) {
		if err := bc.InsertBlock(blk); err != nil && err != ErrKnownBlock {
			t.Fatalf("insert h%d: %v", blk.Height(), err)
		}
	}

	// Branch A: 2 blocks; block 1 sends 100 to bobA.
	brA := newBranch(t, bc, genesis)
	aTx := xfer(t, alice, 0, bobA, 100)
	a1 := brA.next(aTx)
	a2 := brA.next()

	// Branch B: 3 blocks (heavier); block 1 sends 200 to bobB.
	brB := newBranch(t, bc, genesis)
	bTx := xfer(t, alice, 0, bobB, 200)
	b1 := brB.next(bTx)
	b2 := brB.next()
	b3 := brB.next()

	// A takes hold.
	insert(a1)
	insert(a2)
	if bc.Head().Hash() != a2.Hash() {
		t.Fatal("branch A should be the head")
	}
	if bc.StateSnapshot().Balance(bobA).Int64() != 100 {
		t.Fatalf("bobA balance on A = %s, want 100", bc.StateSnapshot().Balance(bobA))
	}

	// B is heavier -> reorg.
	insert(b1)
	insert(b2)
	insert(b3)
	if bc.Head().Hash() != b3.Hash() {
		t.Fatal("heavier branch B should have won the reorg")
	}

	snap := bc.StateSnapshot()
	if snap.Balance(bobB).Int64() != 200 {
		t.Fatalf("bobB balance after reorg = %s, want 200", snap.Balance(bobB))
	}
	if snap.Balance(bobA).Sign() != 0 {
		t.Fatalf("bobA balance after reorg = %s, want 0 — A's tx was NOT unwound (phantom coins)", snap.Balance(bobA))
	}
	if snap.Nonce(alice.Address()) != 1 {
		t.Fatalf("alice nonce after reorg = %d, want 1 (only B's tx counts)", snap.Nonce(alice.Address()))
	}

	// The index follows the reorg: A's tx gone, B's tx present.
	if _, _, err := bc.TxByHash(aTx.Hash()); !errors.Is(err, ErrUnknownTx) {
		t.Fatalf("A's tx still indexed after reorg (err=%v) — a light client would trust a rolled-back tx", err)
	}
	if _, _, err := bc.TxByHash(bTx.Hash()); err != nil {
		t.Fatalf("B's tx not indexed after reorg: %v", err)
	}
}

// Property version: build a random tree of tx-carrying blocks in an order that
// forces many reorgs, then prove the canonical state is byte-identical to a clean
// re-execution of only the canonical chain. Residue from an abandoned branch (a
// balance, nonce, or index entry the reorg forgot to undo) shows up as a state-root
// mismatch.
func TestReorgLeavesNoResidue(t *testing.T) {
	for _, seed := range []int64{1, 5, 9, 13} {
		seed := seed
		t.Run("seed_"+itoa(seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))

			const funders = 50
			keys := make([]*crypto.PrivateKey, funders)
			addrs := make([]common.Address, funders)
			for i := range keys {
				keys[i] = newKey(t)
				addrs[i] = keys[i].Address()
			}
			g := testGenesis(addrs...)
			bc := NewMemBlockchain(g)

			blocks := []*types.Block{bc.Head()} // includes genesis
			usedFunder := 0

			for round := 0; round < 70; round++ {
				// Fork from a random existing block (all within retention).
				parent := blocks[r.Intn(len(blocks))]
				br := newBranch(t, bc, parent)

				var txs []*types.Transaction
				if usedFunder < funders {
					to := addrs[r.Intn(funders)]
					txs = append(txs, xfer(t, keys[usedFunder], 0, to, int64(1+r.Intn(1000))))
					usedFunder++
				}

				blk := br.next(txs...)
				if err := bc.InsertBlock(blk); err != nil && err != ErrKnownBlock {
					t.Fatalf("seed %d: insert h%d: %v", seed, blk.Height(), err)
				}
				blocks = append(blocks, blk)
			}

			// Re-execute only the canonical chain on a pristine node.
			bc2 := NewMemBlockchain(g)
			for h := uint64(1); h <= bc.Head().Height(); h++ {
				blk, err := bc.BlockByHeight(h)
				if err != nil {
					t.Fatalf("seed %d: missing canonical block %d: %v", seed, h, err)
				}
				if err := bc2.InsertBlock(blk); err != nil && err != ErrKnownBlock {
					t.Fatalf("seed %d: scratch insert h%d: %v", seed, h, err)
				}
			}

			if bc2.Head().Hash() != bc.Head().Hash() {
				t.Fatalf("seed %d: scratch head != reorged head", seed)
			}
			if got, want := bc.StateSnapshot().Root(), bc2.StateSnapshot().Root(); got != want {
				t.Fatalf("seed %d: reorg left RESIDUE — canonical root %s != clean re-execution %s",
					seed, got.Hex(), want.Hex())
			}
			t.Logf("seed %d: %d blocks built, canonical height %d, no residue",
				seed, len(blocks)-1, bc.Head().Height())
		})
	}
}

// Closes the loophole the in-memory residue test cannot see: StateSnapshot returns
// the freshly-computed head state, blind to whether the reorg wrote the correct
// on-disk reverse diffs. Reopening the chain forces reconstruction from those diffs
// and checks it matches. A reorg that forgets to unwind the disk passes every
// in-memory check and corrupts state on the next restart.
func TestReorgStatePersistsAcrossRestart(t *testing.T) {
	for _, seed := range []int64{2, 6} {
		seed := seed
		t.Run("seed_"+itoa(seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			const funders = 50
			keys := make([]*crypto.PrivateKey, funders)
			addrs := make([]common.Address, funders)
			for i := range keys {
				keys[i] = newKey(t)
				addrs[i] = keys[i].Address()
			}
			g := testGenesis(addrs...)

			db := store.NewMemory()
			bc, err := NewBlockchain(db, g, Options{})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			reorgStress(t, bc, keys, addrs, r, 70)

			wantHead := bc.Head().Hash()
			wantRoot := bc.StateSnapshot().Root()
			if err := bc.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			// Reopen over the same db: resume() rebuilds head + state from disk.
			bc2, err := NewBlockchain(db, g, Options{})
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			if bc2.Head().Hash() != wantHead {
				t.Fatalf("seed %d: head not persisted across restart", seed)
			}
			if got := bc2.StateSnapshot().Root(); got != wantRoot {
				t.Fatalf("seed %d: reconstructed state %s != in-memory %s — reorg left DISK residue",
					seed, got.Hex(), wantRoot.Hex())
			}
			t.Logf("seed %d: state survived restart at height %d", seed, bc2.Head().Height())
		})
	}
}
