package types

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
)

func mustKey(t *testing.T) *crypto.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSignRecoverRoundtrip(t *testing.T) {
	key := mustKey(t)
	tx := NewTransaction(1, 0, common.Address{0xaa}, big.NewInt(100), IntrinsicGas, big.NewInt(1), nil)
	if err := tx.Sign(key); err != nil {
		t.Fatal(err)
	}
	got, err := tx.Sender()
	if err != nil {
		t.Fatal(err)
	}
	if got != key.Address() {
		t.Fatalf("sender mismatch: got %s want %s", got, key.Address())
	}
}

// The sender is derived, not claimed: flipping any signed field after signing
// must change the recovered sender or fail, never keep the original.
func TestTamperingChangesSender(t *testing.T) {
	key := mustKey(t)
	tx := NewTransaction(1, 0, common.Address{0xaa}, big.NewInt(100), IntrinsicGas, big.NewInt(1), nil)
	if err := tx.Sign(key); err != nil {
		t.Fatal(err)
	}
	original := key.Address()

	// Attacker inflates the value after signing.
	tampered := &Transaction{
		ChainID: tx.ChainID, Nonce: tx.Nonce, To: tx.To,
		Value:    big.NewInt(999999),
		GasLimit: tx.GasLimit, GasPrice: tx.GasPrice, Data: tx.Data, Sig: tx.Sig,
	}
	got, err := tampered.Sender()
	if err == nil && got == original {
		t.Fatal("tampered tx still recovers to the original sender")
	}
}

// Pre-EIP-155 Ethereum let a tx signed on mainnet be replayed on any fork.
// ChainID is inside SigningHash precisely to prevent that.
func TestChainIDReplayProtection(t *testing.T) {
	key := mustKey(t)
	tx := NewTransaction(1, 0, common.Address{0xaa}, big.NewInt(100), IntrinsicGas, big.NewInt(1), nil)
	if err := tx.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := tx.SanityCheck(2); err != ErrChainID {
		t.Fatalf("tx from chain 1 accepted on chain 2: %v", err)
	}
}

func TestSigningHashDeterministic(t *testing.T) {
	build := func() *Transaction {
		return NewTransaction(7, 3, common.Address{0xbe, 0xef}, big.NewInt(1234), 21000, big.NewInt(9), []byte("hi"))
	}
	if build().SigningHash() != build().SigningHash() {
		t.Fatal("signing hash is not deterministic")
	}
}

// Canonical encoding must not be ambiguous: two logically different txs
// must never share a signing hash. This is the length-prefix guarantee.
func TestEncodingUnambiguous(t *testing.T) {
	a := NewTransaction(1, 0, common.Address{0x01}, big.NewInt(0), 21000, big.NewInt(1), []byte("ab"))
	b := NewTransaction(1, 0, common.Address{0x01}, big.NewInt(0), 21000, big.NewInt(1), []byte("a"))
	b.Data = []byte("ab")[:1]
	if a.SigningHash() == b.SigningHash() {
		t.Fatal("different payloads collide")
	}
}

func TestUnsignedTxRejected(t *testing.T) {
	tx := NewTransaction(1, 0, common.Address{0xaa}, big.NewInt(1), IntrinsicGas, big.NewInt(1), nil)
	if _, err := tx.Sender(); err != ErrTxUnsigned {
		t.Fatalf("expected ErrTxUnsigned, got %v", err)
	}
}
