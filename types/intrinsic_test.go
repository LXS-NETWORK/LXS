package types

import "testing"

// Calldata must be priced per byte (EIP-2028) and a creation must carry the 32000
// surcharge. Without the per-byte charge, an attacker fills blocks with cheap
// maximal-data txs the whole network stores and gossips for a flat 21000.
func TestIntrinsicGasFor(t *testing.T) {
	if g := IntrinsicGasFor(nil, false); g != IntrinsicGas {
		t.Fatalf("empty transfer = %d, want %d", g, IntrinsicGas)
	}
	if g := IntrinsicGasFor(nil, true); g != TxGasContractCreation {
		t.Fatalf("empty create = %d, want %d (missing the 32000 surcharge)", g, TxGasContractCreation)
	}
	// one non-zero + one zero byte.
	if g := IntrinsicGasFor([]byte{0x01, 0x00}, false); g != IntrinsicGas+TxDataNonZeroGas+TxDataZeroGas {
		t.Fatalf("2-byte data = %d, want %d", g, IntrinsicGas+TxDataNonZeroGas+TxDataZeroGas)
	}
	// Larger calldata costs strictly more — the DoS defense.
	if IntrinsicGasFor(make([]byte, 1000), false) <= IntrinsicGas {
		t.Fatal("large calldata must cost more than the flat base, or spamming it is free")
	}
}
