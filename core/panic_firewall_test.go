package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/state"
	"lxs/types"
)

// The panic firewall must convert a panic during tx execution into an error, not a
// process crash — otherwise a poison tx/block kills the miner (and, via gossip, every
// node). A signed tx with a nil Value makes ApplyTx's Cost() panic on the nil big.Int.
func TestApplyTxSafeRecoversPanic(t *testing.T) {
	s := state.New()
	k := newKey(t)
	s.Credit(k.Address(), common.LXS(1))

	tx := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: testChainID, Nonce: 0,
		To: &common.Address{0x01}, Value: nil, GasLimit: 21000, GasPrice: big.NewInt(1),
	}
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}

	// Must return an error, not panic the test process.
	if _, _, _, err := applyTxSafe(s, tx, common.Address{}, 10_000_000); err == nil {
		t.Fatal("panic firewall did not surface the panic as an error")
	}
}
