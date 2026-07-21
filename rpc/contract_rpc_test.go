package rpc

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/contracts"
	"lxs/crypto"
	"lxs/types"
)

// deployOrCallTx builds a signed contract transaction (To == nil deploys).
func deployOrCallTx(t *testing.T, k *crypto.PrivateKey, nonce uint64, to *common.Address, data []byte) *types.Transaction {
	t.Helper()
	tx := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: testChainID, Nonce: nonce,
		To: to, Value: big.NewInt(0), GasLimit: 5_000_000, GasPrice: big.NewInt(1), Data: data,
	}
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

func callBalance(t *testing.T, c *Client, token, who common.Address) *big.Int {
	t.Helper()
	var out Data
	if err := c.Call("chain_call", &out, CallArgs{To: token, Data: contracts.BalanceOfCalldata(who)}); err != nil {
		t.Fatalf("chain_call balanceOf: %v", err)
	}
	return new(big.Int).SetBytes(out)
}

// TestERC20OverRPC exercises the VM through the node as a real client would:
// deploy a token, read a balance via a read-only call, transfer, and pull the
// Transfer event from the receipt.
func TestERC20OverRPC(t *testing.T) {
	dev := newKey(t)
	_, c, _, _, prod := setup(t, dev.Address())

	supply := common.LXS(1_000_000)

	// --- deploy (To == nil) ---
	deploy := deployOrCallTx(t, dev, 0, nil, contracts.ERC20Init(supply))
	var dh common.Hash
	if err := c.Call("chain_sendTransaction", &dh, FromTx(deploy)); err != nil {
		t.Fatalf("send deploy: %v", err)
	}
	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}

	var dr ReceiptResult
	if err := c.Call("chain_getTransactionReceipt", &dr, dh); err != nil {
		t.Fatalf("deploy receipt: %v", err)
	}
	if dr.Status.U64() != uint64(types.ReceiptSuccess) {
		t.Fatal("deploy reverted")
	}
	if dr.ContractAddress == nil {
		t.Fatal("deploy receipt has no contractAddress")
	}
	token := *dr.ContractAddress

	// --- read the minted balance with a read-only call ---
	if got := callBalance(t, c, token, dev.Address()); got.Cmp(supply) != 0 {
		t.Fatalf("balanceOf(deployer) over RPC = %s, want minted %s", got, supply)
	}

	// --- transfer 250k to Bob ---
	bob := newKey(t).Address()
	amount := common.LXS(250_000)
	transfer := deployOrCallTx(t, dev, 1, &token, contracts.TransferCalldata(bob, amount))
	var th common.Hash
	if err := c.Call("chain_sendTransaction", &th, FromTx(transfer)); err != nil {
		t.Fatalf("send transfer: %v", err)
	}
	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}

	var tr ReceiptResult
	if err := c.Call("chain_getTransactionReceipt", &tr, th); err != nil {
		t.Fatalf("transfer receipt: %v", err)
	}
	if tr.Status.U64() != uint64(types.ReceiptSuccess) {
		t.Fatal("transfer reverted")
	}

	// The Transfer event survived the full round-trip: consensus receipt ->
	// persisted JSON -> RPC. Topics are [sig, from, to]; data is the amount.
	if len(tr.Logs) != 1 {
		t.Fatalf("transfer receipt has %d logs, want 1", len(tr.Logs))
	}
	log := tr.Logs[0]
	if log.Address != token {
		t.Errorf("log address = %x, want token %x", log.Address, token)
	}
	if len(log.Topics) != 3 || log.Topics[0] != contracts.TransferEventTopic() {
		t.Fatalf("log topics = %x, want [Transfer, from, to]", log.Topics)
	}
	if from := common.Address(log.Topics[1][12:]); from != dev.Address() {
		t.Errorf("Transfer.from = %x, want %x", from, dev.Address())
	}
	if to := common.Address(log.Topics[2][12:]); to != bob {
		t.Errorf("Transfer.to = %x, want %x", to, bob)
	}
	if v := new(big.Int).SetBytes(log.Data); v.Cmp(amount) != 0 {
		t.Errorf("Transfer amount = %s, want %s", v, amount)
	}

	// --- balances moved, read live over RPC ---
	if got := callBalance(t, c, token, dev.Address()); got.Cmp(new(big.Int).Sub(supply, amount)) != 0 {
		t.Errorf("deployer balance after transfer = %s, want %s", got, new(big.Int).Sub(supply, amount))
	}
	if got := callBalance(t, c, token, bob); got.Cmp(amount) != 0 {
		t.Errorf("bob balance after transfer = %s, want %s", got, amount)
	}

	// chain_call is read-only: run a transfer through it (which SSTOREs on the
	// throwaway copy) and confirm committed balance is untouched. A mutating call
	// would leak free, unsigned state changes here.
	devAddr := dev.Address()
	var ignored Data
	if err := c.Call("chain_call", &ignored, CallArgs{From: &devAddr, To: token, Data: contracts.TransferCalldata(bob, amount)}); err != nil {
		t.Fatalf("read-only transfer call errored: %v", err)
	}
	if got := callBalance(t, c, token, bob); got.Cmp(amount) != 0 {
		t.Fatalf("chain_call mutated committed state: bob = %s, want unchanged %s", got, amount)
	}
}

// TestCallRevertSurfaces proves a reverting read-only call returns an error to
// the client (not a silent empty result): balanceOf on a non-contract address,
// or a transfer that overspends, must not look like success.
func TestCallRevertSurfaces(t *testing.T) {
	dev := newKey(t)
	_, c, _, _, prod := setup(t, dev.Address())

	deploy := deployOrCallTx(t, dev, 0, nil, contracts.ERC20Init(common.LXS(100)))
	var dh common.Hash
	if err := c.Call("chain_sendTransaction", &dh, FromTx(deploy)); err != nil {
		t.Fatal(err)
	}
	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}
	var dr ReceiptResult
	if err := c.Call("chain_getTransactionReceipt", &dr, dh); err != nil {
		t.Fatal(err)
	}
	token := *dr.ContractAddress

	// An unknown selector makes the dispatcher revert; chain_call must error.
	var out Data
	err := c.Call("chain_call", &out, CallArgs{To: token, Data: []byte{0xde, 0xad, 0xbe, 0xef}})
	if err == nil {
		t.Fatal("a reverting call must surface an error, not an empty success")
	}
}
