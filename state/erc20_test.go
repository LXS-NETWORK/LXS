package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/contracts"
	"lxs/types"
)

// A real ERC-20 (balances mapping, affordability-checked transfer, Transfer
// event) deployed and driven end to end through ApplyTx, using the shared
// reference token in lxs/contracts so tests and the devnet demo run the same code.

func TestERC20DeployAndTransfer(t *testing.T) {
	transferSig := contracts.TransferEventTopic()

	// transfer selector = keccak256("transfer(address,uint256)")[:4] = 0xa9059cbb,
	// a known-answer tying the dispatcher to the canonical ABI.
	sel := common.Keccak256([]byte("transfer(address,uint256)"))
	if b := sel.Bytes(); b[0] != 0xa9 || b[1] != 0x05 || b[2] != 0x9c || b[3] != 0xbb {
		t.Fatalf("transfer selector = %x, want a9059cbb", b[:4])
	}

	supply := common.LXS(1_000_000)
	initCode := contracts.ERC20Init(supply)

	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(10)) // gas money

	// --- deploy ---
	deploy := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 5_000_000, GasPrice: big.NewInt(1), Data: initCode,
	}
	if err := deploy.Sign(dev); err != nil {
		t.Fatal(err)
	}
	if _, st, _, err := ApplyTx(s, deploy, common.Address{}, 30_000_000); err != nil || st != types.ReceiptSuccess {
		t.Fatalf("ERC-20 deploy failed: st=%d err=%v", st, err)
	}
	token := CreateAddress(dev.Address(), 0)

	// the whole supply was minted to dev.
	if got := s.GetStorage(token, contracts.BalanceSlot(dev.Address())); new(big.Int).SetBytes(got.Bytes()).Cmp(supply) != 0 {
		t.Fatalf("deployer balance = %s, want minted supply %s", new(big.Int).SetBytes(got.Bytes()), supply)
	}

	// --- transfer 250k to Bob ---
	bob := key(t).Address()
	amount := common.LXS(250_000)
	call := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 1, To: &token,
		Value: big.NewInt(0), GasLimit: 5_000_000, GasPrice: big.NewInt(1),
		Data: contracts.TransferCalldata(bob, amount),
	}
	if err := call.Sign(dev); err != nil {
		t.Fatal(err)
	}
	_, st, logs, err := ApplyTx(s, call, common.Address{}, 30_000_000)
	if err != nil || st != types.ReceiptSuccess {
		t.Fatalf("transfer failed: st=%d err=%v", st, err)
	}

	// balances moved exactly.
	wantDev := new(big.Int).Sub(supply, amount)
	if got := new(big.Int).SetBytes(s.GetStorage(token, contracts.BalanceSlot(dev.Address())).Bytes()); got.Cmp(wantDev) != 0 {
		t.Errorf("sender balance = %s, want %s", got, wantDev)
	}
	if got := new(big.Int).SetBytes(s.GetStorage(token, contracts.BalanceSlot(bob)).Bytes()); got.Cmp(amount) != 0 {
		t.Errorf("recipient balance = %s, want %s", got, amount)
	}

	// a Transfer event was emitted with the right topics and data.
	if len(logs) != 1 {
		t.Fatalf("transfer emitted %d logs, want 1 (Transfer)", len(logs))
	}
	log := logs[0]
	if log.Address != token {
		t.Errorf("log address = %x, want token %x", log.Address, token)
	}
	if len(log.Topics) != 3 || log.Topics[0] != transferSig {
		t.Fatalf("topics = %x, want [Transfer-sig, from, to]", log.Topics)
	}
	if got := common.Address(log.Topics[1][12:]); got != dev.Address() {
		t.Errorf("Transfer.from = %x, want %x", got, dev.Address())
	}
	if got := common.Address(log.Topics[2][12:]); got != bob {
		t.Errorf("Transfer.to = %x, want %x", got, bob)
	}
	if v := new(big.Int).SetBytes(log.Data); v.Cmp(amount) != 0 {
		t.Errorf("Transfer amount in data = %s, want %s", v, amount)
	}
}

// TestERC20TransferInsufficientReverts: a transfer for more than the sender holds
// reverts — balances unchanged, no event, failed receipt.
func TestERC20TransferInsufficientReverts(t *testing.T) {
	supply := common.LXS(100)

	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(10))
	deploy := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 5_000_000, GasPrice: big.NewInt(1), Data: contracts.ERC20Init(supply),
	}
	deploy.Sign(dev)
	if _, st, _, err := ApplyTx(s, deploy, common.Address{}, 30_000_000); err != nil || st != types.ReceiptSuccess {
		t.Fatalf("deploy failed: st=%d err=%v", st, err)
	}
	token := CreateAddress(dev.Address(), 0)

	bob := key(t).Address()
	overspend := common.LXS(101) // one more than the whole supply
	call := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 1, To: &token,
		Value: big.NewInt(0), GasLimit: 5_000_000, GasPrice: big.NewInt(1),
		Data: contracts.TransferCalldata(bob, overspend),
	}
	call.Sign(dev)
	_, st, logs, err := ApplyTx(s, call, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("a reverting transfer must not error the block: %v", err)
	}
	if st != types.ReceiptFailed {
		t.Fatal("overspending transfer should revert (failed receipt)")
	}
	if len(logs) != 0 {
		t.Fatalf("a reverted transfer emitted %d logs, want 0", len(logs))
	}
	if got := new(big.Int).SetBytes(s.GetStorage(token, contracts.BalanceSlot(dev.Address())).Bytes()); got.Cmp(supply) != 0 {
		t.Errorf("sender balance after revert = %s, want untouched %s", got, supply)
	}
	if got := s.GetStorage(token, contracts.BalanceSlot(bob)); !got.IsZero() {
		t.Errorf("recipient balance after revert = %x, want 0", got)
	}
}
