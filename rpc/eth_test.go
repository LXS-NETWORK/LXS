package rpc

import (
	"math/big"
	"strconv"
	"testing"

	"lxs/common"
	"lxs/contracts"
)

// TestEthNamespaceReads covers the MetaMask-connect path: the read-only eth_
// methods a wallet calls to identify the network, show a balance, and read a
// contract, all in the hex shape tooling expects.
func TestEthNamespaceReads(t *testing.T) {
	dev := newKey(t)
	_, c, _, _, prod := setup(t, dev.Address())

	// eth_chainId — hex quantity, must equal the chain's id.
	var chainID Quantity
	if err := c.Call("eth_chainId", &chainID); err != nil {
		t.Fatalf("eth_chainId: %v", err)
	}
	if chainID.U64() != testChainID {
		t.Fatalf("eth_chainId = %d, want %d", chainID.U64(), testChainID)
	}

	// net_version — the decimal twin of the chain id.
	var netv string
	if err := c.Call("net_version", &netv); err != nil {
		t.Fatalf("net_version: %v", err)
	}
	if want := strconv.FormatUint(testChainID, 10); netv != want {
		t.Fatalf("net_version = %q, want %q", netv, want)
	}

	// eth_getBalance [addr, "latest"] — MetaMask always sends the block tag.
	var bal Quantity
	if err := c.Call("eth_getBalance", &bal, dev.Address(), "latest"); err != nil {
		t.Fatalf("eth_getBalance: %v", err)
	}
	if bal.Sign() <= 0 {
		t.Fatalf("eth_getBalance = %s, want the funded balance", bal.Int)
	}

	// eth_getTransactionCount and eth_gasPrice.
	var nonce, gasPrice Quantity
	if err := c.Call("eth_getTransactionCount", &nonce, dev.Address(), "latest"); err != nil {
		t.Fatalf("eth_getTransactionCount: %v", err)
	}
	if nonce.U64() != 0 {
		t.Fatalf("fresh account nonce = %d, want 0", nonce.U64())
	}
	if err := c.Call("eth_gasPrice", &gasPrice); err != nil {
		t.Fatalf("eth_gasPrice: %v", err)
	}

	// --- deploy the ERC-20 and read it over eth_ ---
	supply := common.LXS(1_000_000)
	deploy := deployOrCallTx(t, dev, 0, nil, contracts.ERC20Init(supply))
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

	// eth_getCode — proves the address is a contract.
	var code Data
	if err := c.Call("eth_getCode", &code, token, "latest"); err != nil {
		t.Fatalf("eth_getCode: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("eth_getCode returned empty for a deployed contract")
	}

	// eth_call balanceOf(dev) — the standard read path a dapp uses.
	var out Data
	call := ethCallObject{To: &token, Data: contracts.BalanceOfCalldata(dev.Address())}
	if err := c.Call("eth_call", &out, call, "latest"); err != nil {
		t.Fatalf("eth_call balanceOf: %v", err)
	}
	if got := new(big.Int).SetBytes(out); got.Cmp(supply) != 0 {
		t.Fatalf("eth_call balanceOf = %s, want minted %s", got, supply)
	}

	// eth_getBlockByNumber("latest", false) — MetaMask polls this to follow the
	// chain; it must carry number + hash in the Ethereum shape.
	var blk map[string]interface{}
	if err := c.Call("eth_getBlockByNumber", &blk, "latest", false); err != nil {
		t.Fatalf("eth_getBlockByNumber: %v", err)
	}
	if blk["number"] == nil || blk["hash"] == nil {
		t.Fatalf("eth_getBlockByNumber missing number/hash: %v", blk)
	}
}
