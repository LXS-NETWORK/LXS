package types

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
)

func typedTx(t *testing.T, k *crypto.PrivateKey, typ TxType) *Transaction {
	t.Helper()
	to := common.Address{0xaa}
	tx := NewTransaction(1337, 0, to, big.NewInt(100), IntrinsicGas, big.NewInt(1), nil)
	tx.Type = typ
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestUnknownTxTypeIsRejected(t *testing.T) {
	k, _ := crypto.GenerateKey()

	// A properly signed transaction of an unknown type must still fail.
	// The signature proves who sent it, not that it means anything.
	for _, typ := range []TxType{0x01, 0x02, 0x7f, 0xff} {
		tx := typedTx(t, k, typ)
		if err := tx.SanityCheck(1337); err == nil {
			t.Fatalf("type 0x%02x was accepted — an old node ignoring a type a new node executes is a chain split", typ)
		}
	}

	if err := typedTx(t, k, TxTypeTransfer).SanityCheck(1337); err != nil {
		t.Fatalf("the one known type was rejected: %v", err)
	}
}

// The type must be inside the signature; otherwise an attacker rewrites it in
// flight while the transaction still verifies.
func TestTypeIsCoveredBySignature(t *testing.T) {
	k, _ := crypto.GenerateKey()
	tx := typedTx(t, k, TxTypeTransfer)

	sender, err := tx.Sender()
	if err != nil {
		t.Fatal(err)
	}
	if sender != k.Address() {
		t.Fatal("honest tx does not recover to the signer")
	}

	// Rewrite the type, keeping the signature. Rebuild rather than copy: the
	// atomic.Value caches must not carry over, or the test checks the cache.
	tampered := &Transaction{
		Type: 0x01, ChainID: tx.ChainID, Nonce: tx.Nonce, To: tx.To,
		Value: tx.Value, GasLimit: tx.GasLimit, GasPrice: tx.GasPrice,
		Data: tx.Data, Sig: tx.Sig,
	}

	got, err := tampered.Sender()
	if err == nil && got == sender {
		t.Fatal("the type is not covered by the signature — it can be rewritten in flight")
	}
}

// Two transactions identical except for type must have different digests.
// Otherwise a signature for one shape is a valid signature for another.
func TestTypeChangesTheSigningHash(t *testing.T) {
	to := common.Address{0xbb}
	a := NewTransaction(1337, 0, to, big.NewInt(5), IntrinsicGas, big.NewInt(1), nil)
	b := NewTransaction(1337, 0, to, big.NewInt(5), IntrinsicGas, big.NewInt(1), nil)
	b.Type = 0x01

	if a.SigningHash() == b.SigningHash() {
		t.Fatal("type does not affect the signing hash — signatures are replayable across types")
	}
}

func TestDefaultTypeIsTransfer(t *testing.T) {
	tx := NewTransaction(1337, 0, common.Address{0x01}, big.NewInt(1), IntrinsicGas, big.NewInt(1), nil)
	if tx.Type != TxTypeTransfer {
		t.Fatalf("default type: got 0x%02x want 0x00", tx.Type)
	}
}
