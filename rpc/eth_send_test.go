package rpc

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/types"
)

// TestEthSendRawTransaction exercises the MetaMask send path with a real
// EIP-155-signed transaction: build and sign an eth-legacy tx over the EIP-155
// hash, submit the raw bytes to eth_sendRawTransaction, mine, and confirm the
// value moved and the receipt is findable by the Ethereum tx hash.
func TestEthSendRawTransaction(t *testing.T) {
	alice := newKey(t)
	bob := common.Address{0xbb}
	_, c, _, _, prod := setup(t, alice.Address())

	// Build the transaction the way MetaMask does: legacy type, signed over the
	// EIP-155 hash (tx.Sign branches to it because the type is EthLegacy).
	tx := &types.Transaction{
		Type: types.TxTypeEthLegacy, ChainID: testChainID, Nonce: 0, To: &bob,
		Value: big.NewInt(100_000), GasLimit: 21000, GasPrice: big.NewInt(1),
	}
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	raw := tx.EncodeEthRaw()

	// eth_estimateGas for a plain transfer must be the intrinsic 21000.
	var est Quantity
	if err := c.Call("eth_estimateGas", &est, ethCallObject{To: &bob, From: ptr(alice.Address())}); err != nil {
		t.Fatalf("eth_estimateGas: %v", err)
	}
	if est.U64() != types.IntrinsicGas {
		t.Fatalf("eth_estimateGas transfer = %d, want %d", est.U64(), types.IntrinsicGas)
	}

	// eth_sendRawTransaction returns the Ethereum tx hash the wallet tracks.
	var hash common.Hash
	if err := c.Call("eth_sendRawTransaction", &hash, Data(raw)); err != nil {
		t.Fatalf("eth_sendRawTransaction: %v", err)
	}
	if hash != tx.Hash() {
		t.Fatalf("returned hash %s != keccak(raw) %s", hash.Hex(), tx.Hash().Hex())
	}

	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}

	// The value moved to bob: the MetaMask-shaped tx executed.
	var bal Quantity
	if err := c.Call("eth_getBalance", &bal, bob, "latest"); err != nil {
		t.Fatal(err)
	}
	if bal.U64() != 100_000 {
		t.Fatalf("bob balance = %d, want 100000", bal.U64())
	}

	// The receipt is findable by the Ethereum tx hash, with success status.
	var r map[string]interface{}
	if err := c.Call("eth_getTransactionReceipt", &r, hash); err != nil {
		t.Fatalf("eth_getTransactionReceipt: %v", err)
	}
	if r == nil {
		t.Fatal("no receipt for a mined transaction")
	}
	if r["status"] != "0x1" {
		t.Fatalf("receipt status = %v, want 0x1", r["status"])
	}
	// The recovered 'from' is alice: the signature was honoured.
	if r["from"] != alice.Address().Hex() {
		t.Fatalf("receipt from = %v, want %s", r["from"], alice.Address().Hex())
	}
}

func ptr(a common.Address) *common.Address { return &a }
