package types

import (
	"math/big"
	"testing"
)

// A hostile peer can gossip a tx with any Type and any Sig. Hash() runs during dedup
// BEFORE SanityCheck, so it must be total — a malformed eth-legacy signature must never
// panic (it once nil-index-panicked in EncodeEthRaw and crashed the whole node).
func TestHashTotalOnMalformedEthLegacySig(t *testing.T) {
	for _, sig := range [][]byte{nil, {}, {27}, make([]byte, 10), make([]byte, 64), make([]byte, 200)} {
		tx := &Transaction{
			Type: TxTypeEthLegacy, ChainID: 1, Nonce: 1, To: nil,
			Value: big.NewInt(0), GasLimit: 21000, GasPrice: big.NewInt(1), Sig: sig,
		}
		_ = tx.Hash() // must NOT panic
		if got := tx.EncodeEthRaw(); got != nil {
			t.Fatalf("EncodeEthRaw on a %d-byte sig returned non-nil, want nil", len(sig))
		}
	}
	// a well-formed 65-byte sig still takes the raw-encoding path (unchanged behaviour).
	tx := &Transaction{
		Type: TxTypeEthLegacy, ChainID: 1, Nonce: 1, To: nil,
		Value: big.NewInt(0), GasLimit: 21000, GasPrice: big.NewInt(1), Sig: make([]byte, 65),
	}
	tx.Sig[0] = 27
	if tx.EncodeEthRaw() == nil {
		t.Fatal("EncodeEthRaw on a valid 65-byte sig returned nil")
	}
}
