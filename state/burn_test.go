package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
	"lxs/types"
)

func burnKey(t *testing.T) *crypto.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestBurnFoldsIntoRootBackwardCompatible: the burn total binds into the root,
// but a zero burn leaves the root exactly the account merkle, so no existing root
// shifts until the first burn.
func TestBurnFoldsIntoRootBackwardCompatible(t *testing.T) {
	s := New()
	s.Credit(common.Address{1}, big.NewInt(1000))
	r0 := s.Root()

	s.Burn(big.NewInt(100))
	if s.Root() == r0 {
		t.Fatal("a burn must change the state root — else a node could hide it")
	}

	// With the burn total back to zero, the root is again the account merkle.
	s.SetBurned(big.NewInt(0))
	if s.Root() != r0 {
		t.Fatal("zero-burn root must equal the account merkle (no gratuitous root change)")
	}

	// Different totals over the same accounts must root differently, or a node
	// could under-report what it destroyed.
	s.SetBurned(big.NewInt(100))
	rA := s.Root()
	s.SetBurned(big.NewInt(200))
	if s.Root() == rA {
		t.Fatal("different burn totals must yield different roots")
	}
}

// TestBurnRecognizedByApplyTx: a transfer to the burn address destroys the value
// — it leaves the sender, folds into the burn total, and the burn address never
// holds a balance.
func TestBurnRecognizedByApplyTx(t *testing.T) {
	s := New()
	k := burnKey(t)
	from := k.Address()
	s.Credit(from, common.LXS(10))

	tx := types.NewTransaction(1337, 0, common.BurnAddress, common.LXS(3), types.IntrinsicGas, big.NewInt(0), nil)
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	_, status, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
	if err != nil || status != types.ReceiptSuccess {
		t.Fatalf("burn tx: status=%d err=%v", status, err)
	}
	if got := s.Burned(); got.Cmp(common.LXS(3)) != 0 {
		t.Fatalf("TotalBurned = %s, want 3 LXS", got)
	}
	if got := s.Balance(common.BurnAddress); got.Sign() != 0 {
		t.Fatalf("burn address balance = %s, want 0 (destroyed, not credited)", got)
	}
	if got := s.Balance(from); got.Cmp(common.LXS(7)) != 0 { // gasPrice 0, so only value left
		t.Fatalf("sender balance = %s, want 7 LXS", got)
	}
}

// TestBurnRolledBackOnRevert: the burn total rides the snapshot stack, so a burn
// inside a reverted frame is undone exactly like a balance change.
func TestBurnRolledBackOnRevert(t *testing.T) {
	s := New()
	s.Burn(big.NewInt(50))
	id := s.Snapshot()
	s.Burn(big.NewInt(30)) // total 80 inside the frame
	s.RevertToSnapshot(id)
	if got := s.Burned(); got.Int64() != 50 {
		t.Fatalf("burned after revert = %d, want 50 (the reverted burn is undone)", got.Int64())
	}
}

// TestBurnCarriesAcrossCopy: Copy() must carry the burn total, or the producer's
// per-tx Copy() would forget earlier burns and the block's root would omit them.
func TestBurnCarriesAcrossCopy(t *testing.T) {
	s := New()
	s.Burn(big.NewInt(42))
	c := s.Copy()
	if got := c.Burned(); got.Int64() != 42 {
		t.Fatalf("copy burned = %d, want 42", got.Int64())
	}
	c.Burn(big.NewInt(8)) // mutate the copy only
	if got := s.Burned(); got.Int64() != 42 {
		t.Fatalf("original burned = %d, want 42 (copy must be independent)", got.Int64())
	}
}

// TestConservationWithBurn: value is conserved except for what was burned.
// sum(balances) + burned is invariant: nothing is created, and destruction is only
// ever the tracked burn.
func TestConservationWithBurn(t *testing.T) {
	s := New()
	a, b := burnKey(t), burnKey(t)
	s.Credit(a.Address(), common.LXS(100))
	s.Credit(b.Address(), common.LXS(100))
	initial := common.LXS(200)

	// gasPrice 0 so no fee leaves the accounting; a couple of transfers, one a burn.
	send := func(k *crypto.PrivateKey, nonce uint64, to common.Address, v *big.Int) {
		tx := types.NewTransaction(1337, nonce, to, v, types.IntrinsicGas, big.NewInt(0), nil)
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		if _, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000); err != nil || st != types.ReceiptSuccess {
			t.Fatalf("apply: st=%d err=%v", st, err)
		}
	}
	send(a, 0, b.Address(), common.LXS(20))        // a -> b
	send(a, 1, common.BurnAddress, common.LXS(30)) // a burns 30
	send(b, 0, common.BurnAddress, common.LXS(5))  // b burns 5

	sum := new(big.Int)
	for _, acc := range s.Accounts() {
		sum.Add(sum, acc.Balance)
	}
	sum.Add(sum, s.Burned())
	if sum.Cmp(initial) != 0 {
		t.Fatalf("sum(balances)+burned = %s, want %s (conservation broken)", sum, initial)
	}
	if got := s.Burned(); got.Cmp(common.LXS(35)) != 0 {
		t.Fatalf("burned = %s, want 35 LXS", got)
	}
}
