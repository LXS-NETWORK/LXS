package state

import (
	"errors"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/types"
)

// A tx may not claim more gas than a whole block. Without this a single tx with a huge
// GasLimit running an infinite loop executes to completion inside ApplyTx, hanging every
// validating node before ApplyBlock's post-loop total-gas check is reached.
func TestApplyTxRejectsGasAboveBlockLimit(t *testing.T) {
	s := New()
	k := key(t)
	s.Credit(k.Address(), common.LXS(1000))
	to := common.Address{0xAB}

	poison := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: &to,
		Value: big.NewInt(0), GasLimit: 1 << 62, GasPrice: big.NewInt(1),
	}
	if err := poison.Sign(k); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ApplyTx(s, poison, common.Address{}, 30_000_000); err == nil {
		t.Fatal("SECURITY: a tx claiming 2^62 gas (>> block limit) was accepted — an infinite-loop tx would hang every node")
	} else if !errors.Is(err, ErrGasLimit) {
		t.Fatalf("want ErrGasLimit, got %v", err)
	}

	// a tx at exactly the block gas limit is still fine.
	ok := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: &to,
		Value: big.NewInt(0), GasLimit: 30_000_000, GasPrice: big.NewInt(1),
	}
	if err := ok.Sign(k); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ApplyTx(s, ok, common.Address{}, 30_000_000); err != nil {
		t.Fatalf("a tx at exactly the block gas limit was rejected: %v", err)
	}
}
