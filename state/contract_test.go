package state

import (
	"bytes"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/types"
)

// Deployment test: a tx with To == nil and Data == init bytecode creates a
// contract; a second tx to that address runs its code, which SSTOREs a value; the
// value is in the contract's storage afterwards.
func TestContractDeployThenCallStoresValue(t *testing.T) {
	// Runtime code (what the deployed contract runs on every call):
	//   PUSH1 0x00 ; CALLDATALOAD ; PUSH1 0x00 ; SSTORE ; STOP
	//   -> store the first 32 bytes of call data into storage slot 0.
	runtime := []byte{0x60, 0x00, 0x35, 0x60, 0x00, 0x55, 0x00}

	// Init code (the constructor): return the runtime code so it gets stored.
	//   PUSH7 <runtime> ; PUSH1 0x00 ; MSTORE ; PUSH1 0x07 ; PUSH1 0x19 ; RETURN
	//   MSTORE right-aligns the 7 bytes at memory[25:32]; RETURN 7 bytes from 25.
	initCode := []byte{0x66} // PUSH7
	initCode = append(initCode, runtime...)
	initCode = append(initCode,
		0x60, 0x00, // PUSH1 0
		0x52,       // MSTORE
		0x60, 0x07, // PUSH1 7  (size)
		0x60, 0x19, // PUSH1 25 (offset)
		0xf3, // RETURN
	)

	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(1000)) // fund the deployer

	// --- 1. deploy ---
	deploy := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 1_000_000, GasPrice: big.NewInt(1), Data: initCode,
	}
	if err := deploy.Sign(dev); err != nil {
		t.Fatal(err)
	}
	_, status, _, err := ApplyTx(s, deploy, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("deploy tx invalid: %v", err)
	}
	if status != types.ReceiptSuccess {
		t.Fatal("deployment reverted")
	}

	contract := CreateAddress(dev.Address(), 0)
	if !bytes.Equal(s.GetCode(contract), runtime) {
		t.Fatalf("deployed code = %x, want runtime %x", s.GetCode(contract), runtime)
	}

	// --- 2. call it with the value 42 ---
	arg := make([]byte, 32)
	arg[31] = 42
	call := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 1, To: &contract,
		Value: big.NewInt(0), GasLimit: 1_000_000, GasPrice: big.NewInt(1), Data: arg,
	}
	if err := call.Sign(dev); err != nil {
		t.Fatal(err)
	}
	_, status, _, err = ApplyTx(s, call, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("call tx invalid: %v", err)
	}
	if status != types.ReceiptSuccess {
		t.Fatal("contract call reverted")
	}

	// --- 3. the value is in the contract's storage ---
	got := s.GetStorage(contract, common.Hash{})
	if got[31] != 42 {
		t.Fatalf("contract storage slot 0 = %s, want 42", got.Hex())
	}
}

// A contract call that runs out of gas reverts: state rolled back, receipt
// failed, but gas is consumed and the nonce advances — neither free nor
// replayable.
func TestContractCallOutOfGasRevertsButChargesGas(t *testing.T) {
	// Runtime: an infinite loop — JUMPDEST ; PUSH1 0 ; JUMP.
	runtime := []byte{0x5b, 0x60, 0x00, 0x56}
	initCode := []byte{0x63} // PUSH4
	initCode = append(initCode, runtime...)
	initCode = append(initCode, 0x60, 0x00, 0x52, 0x60, 0x04, 0x60, 0x1c, 0xf3) // store & RETURN 4 bytes from 28

	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(1000))

	deploy := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 1_000_000, GasPrice: big.NewInt(1), Data: initCode,
	}
	deploy.Sign(dev)
	if _, st, _, err := ApplyTx(s, deploy, common.Address{}, 30_000_000); err != nil || st != types.ReceiptSuccess {
		t.Fatalf("deploy failed: st=%d err=%v", st, err)
	}
	contract := CreateAddress(dev.Address(), 0)

	balBefore := new(big.Int).Set(s.Balance(dev.Address()))
	call := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 1, To: &contract,
		Value: big.NewInt(0), GasLimit: 100_000, GasPrice: big.NewInt(1), Data: nil,
	}
	call.Sign(dev)
	_, status, _, err := ApplyTx(s, call, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("a reverting call must not error the block: %v", err)
	}
	if status != types.ReceiptFailed {
		t.Fatal("an out-of-gas call should have a failed receipt")
	}
	// Gas was charged (balance dropped) and the nonce advanced.
	if s.Balance(dev.Address()).Cmp(balBefore) >= 0 {
		t.Fatal("a failed call must still cost gas")
	}
	if s.Nonce(dev.Address()) != 2 {
		t.Fatalf("nonce after failed call = %d, want 2", s.Nonce(dev.Address()))
	}
}
