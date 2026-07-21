package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/types"
)

// A data-carrying tx must be rejected if its gas limit only covers the flat base
// (21000) — the calldata bytes have to be paid for. Otherwise maximal-data txs are
// admitted for free.
func TestDataTxRejectedBelowCalldataIntrinsic(t *testing.T) {
	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(1000))
	to := common.Address{0x02}

	tx := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: &to,
		Value: big.NewInt(0), GasLimit: types.IntrinsicGas, GasPrice: big.NewInt(1),
		Data: make([]byte, 100), // 100 zero bytes -> needs 21000 + 400
	}
	if err := tx.Sign(dev); err != nil {
		t.Fatal(err)
	}
	_, status, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
	if err == nil && status == types.ReceiptSuccess {
		t.Fatal("a data-carrying tx funded only for the flat 21000 must be rejected (calldata not priced)")
	}
}
