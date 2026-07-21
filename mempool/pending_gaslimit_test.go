package mempool

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
	"lxs/state"
	"lxs/types"
)

// Pending must size each tx by its own gas limit, not a flat intrinsic. Two txs
// whose limits together exceed the block gas limit cannot both be packed, or the
// producer builds a block its own ApplyBlock rejects for over-gas.
func TestPendingSizesByTxGasLimit(t *testing.T) {
	m := New(100)
	s := state.New()
	k1, k2 := mustKey(t), mustKey(t)
	s.Credit(k1.Address(), common.LXS(1))
	s.Credit(k2.Address(), common.LXS(1))

	mk := func(k *crypto.PrivateKey, gasPrice int64) *types.Transaction {
		tx := types.NewTransaction(testChainID, 0, common.Address{0x01}, big.NewInt(0), 6_000_000, big.NewInt(gasPrice), nil)
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	if err := m.Add(mk(k1, 2), testChainID); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(mk(k2, 1), testChainID); err != nil {
		t.Fatal(err)
	}

	// Two 6M-limit txs = 12M > a 10M block, so exactly one must be selected.
	if got := m.Pending(s, 10_000_000); len(got) != 1 {
		t.Fatalf("Pending returned %d txs; two 6M-limit txs exceed a 10M block, only one fits", len(got))
	}
}

// PendingNonce must advance past consecutively-queued txs so a rapid-firing wallet
// does not reuse a nonce, and must stop at the first gap.
func TestPendingNonceAdvancesPastQueued(t *testing.T) {
	m := New(100)
	k := mustKey(t)
	if got := m.PendingNonce(k.Address(), 3); got != 3 {
		t.Fatalf("no queued txs: pending nonce = %d, want 3", got)
	}
	if err := m.Add(txFrom(t, k, 3, 1, 1), testChainID); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(txFrom(t, k, 4, 1, 1), testChainID); err != nil {
		t.Fatal(err)
	}
	if got := m.PendingNonce(k.Address(), 3); got != 5 {
		t.Fatalf("nonces 3,4 queued: pending = %d, want 5", got)
	}
	// A gap at 5 (queue 6): pending must still stop at 5.
	if err := m.Add(txFrom(t, k, 6, 1, 1), testChainID); err != nil {
		t.Fatal(err)
	}
	if got := m.PendingNonce(k.Address(), 3); got != 5 {
		t.Fatalf("gap at 5: pending = %d, want 5", got)
	}
}
