package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
	"lxs/types"
)

const chainID = 1

func key(t *testing.T) *crypto.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signed(t *testing.T, k *crypto.PrivateKey, nonce uint64, to common.Address, value int64, gasPrice int64) *types.Transaction {
	t.Helper()
	tx := types.NewTransaction(chainID, nonce, to, big.NewInt(value), types.IntrinsicGas, big.NewInt(gasPrice), nil)
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

// State-root determinism. Go randomises map iteration, so if Root() forgets to
// sort this fails quickly. A node that disagrees with itself between restarts
// presents as a network fault.
func TestStateRootDeterministicAcrossInsertionOrder(t *testing.T) {
	addrs := make([]common.Address, 50)
	for i := range addrs {
		addrs[i] = key(t).Address()
	}

	forward := New()
	for i, a := range addrs {
		forward.Credit(a, big.NewInt(int64(i+1)*1000))
	}

	backward := New()
	for i := len(addrs) - 1; i >= 0; i-- {
		backward.Credit(addrs[i], big.NewInt(int64(i+1)*1000))
	}

	if forward.Root() != backward.Root() {
		t.Fatal("state root depends on insertion order")
	}
	for i := 0; i < 50; i++ {
		if forward.Root() != backward.Root() {
			t.Fatal("state root not stable across calls")
		}
	}
}

func TestEmptyAccountDoesNotAffectRoot(t *testing.T) {
	a, b := New(), New()
	addr := key(t).Address()
	a.Credit(addr, big.NewInt(0)) // "touch" an account with nothing
	if a.Root() != b.Root() {
		t.Fatal("touching a zero account changed the state root")
	}
}

func TestCopyIsIndependent(t *testing.T) {
	s := New()
	addr := key(t).Address()
	s.Credit(addr, big.NewInt(100))
	root := s.Root()

	c := s.Copy()
	c.Credit(addr, big.NewInt(900))

	if s.Root() != root {
		t.Fatal("mutating a copy mutated the original")
	}
	if s.Balance(addr).Int64() != 100 {
		t.Fatal("original balance was aliased into the copy")
	}
}

// Value must be neither created nor destroyed by a transfer; fees move to the
// proposer, not evaporate.
func TestConservationOfValue(t *testing.T) {
	alice, bob, miner := key(t), key(t), key(t)
	s := New()
	s.Credit(alice.Address(), big.NewInt(1_000_000))

	total := func() *big.Int {
		// Conserved quantity = balances + burned. A slice of the fee is burned,
		// so balances alone shrink; burned is the other half of the ledger.
		sum := new(big.Int).Set(s.Burned())
		for _, acc := range s.Accounts() {
			sum.Add(sum, acc.Balance)
		}
		return sum
	}
	before := total()

	tx := signed(t, alice, 0, bob.Address(), 1000, 2)
	if _, _, _, err := ApplyTx(s, tx, miner.Address(), 30_000_000); err != nil {
		t.Fatal(err)
	}

	if before.Cmp(total()) != 0 {
		t.Fatalf("conserved supply changed: before %s after %s", before, total())
	}
}

func TestFeeGoesToProposerAndUnusedGasRefunded(t *testing.T) {
	alice, bob, miner := key(t), key(t), key(t)
	s := New()
	s.Credit(alice.Address(), big.NewInt(1_000_000))

	// Deliberately over-provision gas: 3x the intrinsic cost.
	tx := types.NewTransaction(chainID, 0, bob.Address(), big.NewInt(1000), types.IntrinsicGas*3, big.NewInt(2), nil)
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ApplyTx(s, tx, miner.Address(), 30_000_000); err != nil {
		t.Fatal(err)
	}

	// The fee splits: FeeBurnBasisPoints is burned, the rest tips the proposer. The
	// sender still pays the whole fee, so the refund of unused gas is unchanged.
	fullFee := int64(types.IntrinsicGas) * 2
	wantBurn := fullFee * int64(FeeBurnBasisPoints) / 10000
	wantTip := fullFee - wantBurn
	if got := s.Balance(miner.Address()).Int64(); got != wantTip {
		t.Fatalf("proposer tip: got %d want %d (75%% of fee %d)", got, wantTip, fullFee)
	}
	if got := s.Burned().Int64(); got != wantBurn {
		t.Fatalf("fee burn: got %d want %d (20%% of fee %d)", got, wantBurn, fullFee)
	}
	wantAlice := int64(1_000_000) - 1000 - fullFee
	if got := s.Balance(alice.Address()).Int64(); got != wantAlice {
		t.Fatalf("unused gas not refunded: got %d want %d", got, wantAlice)
	}
}

func TestNonceMustMatchExactly(t *testing.T) {
	alice, bob, miner := key(t), key(t), key(t)
	s := New()
	s.Credit(alice.Address(), big.NewInt(1_000_000))

	if _, _, _, err := ApplyTx(s, signed(t, alice, 1, bob.Address(), 10, 1), miner.Address(), 30_000_000); err == nil {
		t.Fatal("future nonce accepted")
	}
	if _, _, _, err := ApplyTx(s, signed(t, alice, 0, bob.Address(), 10, 1), miner.Address(), 30_000_000); err != nil {
		t.Fatal(err)
	}
	// Replay of the same nonce must now fail.
	if _, _, _, err := ApplyTx(s, signed(t, alice, 0, bob.Address(), 10, 1), miner.Address(), 30_000_000); err == nil {
		t.Fatal("replayed nonce accepted")
	}
}

func TestFailedTxLeavesNoTrace(t *testing.T) {
	alice, bob, miner := key(t), key(t), key(t)
	s := New()
	s.Credit(alice.Address(), big.NewInt(100)) // cannot afford anything
	root := s.Root()

	if _, _, _, err := ApplyTx(s, signed(t, alice, 0, bob.Address(), 1_000_000, 1), miner.Address(), 30_000_000); err == nil {
		t.Fatal("expected insufficient balance")
	}
	if s.Root() != root {
		t.Fatal("failed tx mutated state (atomicity violated)")
	}
}

// Spending the whole balance on value leaves nothing for gas; charging max fee up
// front stops it.
func TestCannotSpendGasMoneyOnValue(t *testing.T) {
	alice, bob, miner := key(t), key(t), key(t)
	s := New()
	s.Credit(alice.Address(), big.NewInt(21000)) // exactly the gas, no more

	tx := signed(t, alice, 0, bob.Address(), 21000, 1) // value == entire balance
	if _, _, _, err := ApplyTx(s, tx, miner.Address(), 30_000_000); err == nil {
		t.Fatal("tx that cannot pay for its own gas was accepted")
	}
}
