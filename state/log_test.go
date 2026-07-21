package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
	"lxs/types"
)

// deployEmitter deploys a contract whose runtime is `runtime` and returns its
// address. Shared setup for the log tests.
func deployEmitter(t *testing.T, s *State, dev *crypto.PrivateKey, runtime []byte) common.Address {
	t.Helper()
	// init: PUSHn <runtime> ; PUSH1 0 ; MSTORE ; PUSH1 len ; PUSH1 (32-len) ; RETURN
	n := len(runtime)
	initCode := []byte{byte(0x60 + n - 1)} // PUSH<n>
	initCode = append(initCode, runtime...)
	initCode = append(initCode, 0x60, 0x00, 0x52, 0x60, byte(n), 0x60, byte(32-n), 0xf3)

	deploy := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 1_000_000, GasPrice: big.NewInt(1), Data: initCode,
	}
	if err := deploy.Sign(dev); err != nil {
		t.Fatal(err)
	}
	if _, st, _, err := ApplyTx(s, deploy, common.Address{}, 30_000_000); err != nil || st != types.ReceiptSuccess {
		t.Fatalf("emitter deploy failed: st=%d err=%v", st, err)
	}
	return CreateAddress(dev.Address(), 0)
}

// TestApplyTxCollectsLogs: an event a contract emits surfaces in the tx's logs
// (and receipt) — the path an ERC-20 Transfer event travels.
func TestApplyTxCollectsLogs(t *testing.T) {
	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(1000))

	// Runtime: PUSH1 0 ; PUSH1 0 ; LOG0 ; STOP  (emit an empty, topic-less log).
	emitter := deployEmitter(t, s, dev, []byte{0x60, 0x00, 0x60, 0x00, 0xa0, 0x00})

	call := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 1, To: &emitter,
		Value: big.NewInt(0), GasLimit: 1_000_000, GasPrice: big.NewInt(1), Data: nil,
	}
	if err := call.Sign(dev); err != nil {
		t.Fatal(err)
	}
	_, status, logs, err := ApplyTx(s, call, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("call tx invalid: %v", err)
	}
	if status != types.ReceiptSuccess {
		t.Fatal("emitter call reverted")
	}
	if len(logs) != 1 {
		t.Fatalf("tx produced %d logs, want 1", len(logs))
	}
	if logs[0].Address != emitter {
		t.Errorf("log address = %x, want emitter %x", logs[0].Address, emitter)
	}
}

// TestApplyTxRevertDropsLogs: a contract that emits then reverts must produce no
// logs, or an observer could be convinced of a transfer that was rolled back.
func TestApplyTxRevertDropsLogs(t *testing.T) {
	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(1000))

	// Runtime: LOG0 then REVERT — PUSH1 0;PUSH1 0;LOG0;PUSH1 0;PUSH1 0;REVERT.
	emitter := deployEmitter(t, s, dev, []byte{0x60, 0x00, 0x60, 0x00, 0xa0, 0x60, 0x00, 0x60, 0x00, 0xfd})

	call := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 1, To: &emitter,
		Value: big.NewInt(0), GasLimit: 1_000_000, GasPrice: big.NewInt(1), Data: nil,
	}
	if err := call.Sign(dev); err != nil {
		t.Fatal(err)
	}
	_, status, logs, err := ApplyTx(s, call, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("a reverting call must not error the block: %v", err)
	}
	if status != types.ReceiptFailed {
		t.Fatal("emitter reverted, receipt should be a failure")
	}
	if len(logs) != 0 {
		t.Fatalf("a reverted tx surfaced %d logs, want 0", len(logs))
	}
}
