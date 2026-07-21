package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/types"
)

// EIP-170: a contract whose deployed code exceeds MaxCodeSize (24576) must be
// rejected, not stored. Code lives on every node forever; an unbounded contract is
// a permanent burden, and mainnet refuses it, so we must too or a contract that
// deploys here would be rejected there.
func TestContractDeployRejectedOverMaxCodeSize(t *testing.T) {
	// Init code returns 24577 (MaxCodeSize+1) zero bytes as the runtime code:
	//   PUSH2 0x6001 (size) ; PUSH1 0x00 (offset) ; RETURN
	initCode := []byte{0x61, 0x60, 0x01, 0x60, 0x00, 0xf3}

	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(1000))

	deploy := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 10_000_000, GasPrice: big.NewInt(1), Data: initCode,
	}
	if err := deploy.Sign(dev); err != nil {
		t.Fatal(err)
	}
	_, status, _, err := ApplyTx(s, deploy, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("tx invalid: %v", err)
	}
	if status == types.ReceiptSuccess {
		t.Fatal("a contract exceeding MaxCodeSize (EIP-170) must not deploy successfully")
	}
	if code := s.GetCode(CreateAddress(dev.Address(), 0)); len(code) != 0 {
		t.Fatalf("over-size code was stored anyway: %d bytes", len(code))
	}
}
