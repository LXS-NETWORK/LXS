package mempool

import (
	"errors"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
	"lxs/state"
	"lxs/types"
)

const testChainID = 1337

func mustKey(t *testing.T) *crypto.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func txFrom(t *testing.T, k *crypto.PrivateKey, nonce uint64, value, gasPrice int64) *types.Transaction {
	t.Helper()
	tx := types.NewTransaction(testChainID, nonce, common.Address{}, big.NewInt(value), types.IntrinsicGas, big.NewInt(gasPrice), nil)
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

// A funded account at its current nonce, sending something it can afford,
// passes admission.
func TestCheckStateAcceptsAffordableCurrentNonce(t *testing.T) {
	k := mustKey(t)
	s := state.New()
	s.Credit(k.Address(), big.NewInt(1_000_000))

	if err := CheckState(s, txFrom(t, k, 0, 100, 1)); err != nil {
		t.Fatalf("a funded, current-nonce tx was refused: %v", err)
	}
}

// A future nonce is legitimate: it queues until its predecessors arrive and
// Pending() sequences it. Admission must not reject it.
func TestCheckStateAcceptsFutureNonce(t *testing.T) {
	k := mustKey(t)
	s := state.New()
	s.Credit(k.Address(), big.NewInt(1_000_000))

	if err := CheckState(s, txFrom(t, k, 3, 100, 1)); err != nil {
		t.Fatalf("a future-nonce tx was refused: %v", err)
	}
}

// A nonce below the account's is already spent and can never execute.
func TestCheckStateRejectsStaleNonce(t *testing.T) {
	k := mustKey(t)
	s := state.New()
	s.Credit(k.Address(), big.NewInt(1_000_000))
	s.SetNonce(k.Address(), 5)

	err := CheckState(s, txFrom(t, k, 3, 100, 1))
	if !errors.Is(err, ErrNonceStale) {
		t.Fatalf("stale nonce: got %v, want ErrNonceStale", err)
	}
}

// An account that cannot cover gasLimit*gasPrice + value is refused: the spam
// floor that keeps empty accounts from filling the pool for free.
func TestCheckStateRejectsUnaffordable(t *testing.T) {
	k := mustKey(t)
	s := state.New()
	// Enough for neither the fee nor the value.
	s.Credit(k.Address(), big.NewInt(10))

	// Cost = IntrinsicGas*1 + 100, well over 10.
	err := CheckState(s, txFrom(t, k, 0, 100, 1))
	if !errors.Is(err, ErrCannotPay) {
		t.Fatalf("unaffordable tx: got %v, want ErrCannotPay", err)
	}
}

// TestMinGasPriceAdmissionFloor: with a floor set, underpriced txs are refused at
// admission; at/above the floor they pass; with no floor (default) any price passes.
func TestMinGasPriceAdmissionFloor(t *testing.T) {
	k := mustKey(t)

	// no floor => a gasPrice-0 tx is admitted (consensus-neutral default).
	m0 := New(100)
	if err := m0.Add(txFrom(t, k, 0, 100, 0), testChainID); err != nil {
		t.Fatalf("with no floor, a gasPrice-0 tx was refused: %v", err)
	}

	// floor of 2: price 1 is underpriced, price 2 is accepted.
	m := New(100)
	m.SetMinGasPrice(big.NewInt(2))
	if err := m.Add(txFrom(t, mustKey(t), 0, 100, 1), testChainID); !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("underpriced tx (1 < floor 2) got %v, want ErrUnderpriced", err)
	}
	if err := m.Add(txFrom(t, mustKey(t), 0, 100, 2), testChainID); err != nil {
		t.Fatalf("at-floor tx (2) was refused: %v", err)
	}
}

// TestReinjectBypassesFloor: a below-floor tx orphaned by a reorg (already mined, so
// consensus-acceptable) must be re-pooled by Reinject even though a fresh Add would reject it.
func TestReinjectBypassesFloor(t *testing.T) {
	m := New(100)
	m.SetMinGasPrice(big.NewInt(5))
	tx := txFrom(t, mustKey(t), 0, 100, 1) // priced 1, below the floor of 5

	if err := m.Add(tx, testChainID); !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("fresh Add of a below-floor tx = %v, want ErrUnderpriced", err)
	}
	if got := m.Reinject([]*types.Transaction{tx}, testChainID); got != 1 {
		t.Fatalf("Reinject accepted %d, want 1 (a mined tx must not be lost to the floor)", got)
	}
}
