package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/types"
)

// TestContractCreationCollisionRejected (EIP-684): a creation whose target
// already holds code must fail and leave the existing code untouched.
func TestContractCreationCollisionRejected(t *testing.T) {
	s := New()
	dev := key(t)
	s.Credit(dev.Address(), common.LXS(100))

	// Pre-place code at exactly the address the dev's nonce-0 creation would target.
	addr := CreateAddress(dev.Address(), 0)
	existing := []byte{0x60, 0x00} // some non-empty runtime
	s.SetCode(addr, existing)

	tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 200_000, GasPrice: big.NewInt(1), Data: []byte{0x00}}
	if err := tx.Sign(dev); err != nil {
		t.Fatal(err)
	}
	_, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("tx errored the block: %v", err)
	}
	if st != types.ReceiptFailed {
		t.Fatalf("create over existing code = status %d, want failed", st)
	}
	if code := s.GetCode(addr); len(code) != len(existing) || code[0] != existing[0] {
		t.Fatalf("existing contract code was overwritten: %x", code)
	}
}
