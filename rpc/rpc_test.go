package rpc

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/types"
)

const testChainID = 1337

func newKey(t *testing.T) *crypto.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func setup(t *testing.T, funded ...common.Address) (*httptest.Server, *Client, *core.Blockchain, *mempool.Mempool, *core.Producer) {
	t.Helper()
	alloc := make(map[common.Address]*core.BigStr)
	supply, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 10^24
	for _, a := range funded {
		alloc[a] = &core.BigStr{Int: new(big.Int).Set(supply)}
	}
	g := &core.Genesis{ChainID: testChainID, Timestamp: 1700000000000, GasLimit: 30_000_000, Alloc: alloc}

	bc := core.NewMemBlockchain(g)
	pool := mempool.New(1024)
	miner := newKey(t)
	prod := core.NewProducer(bc, pool, miner.Address())

	srv := NewServer()
	NewAPI(bc, pool).Register(srv)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	return ts, NewClient(ts.URL), bc, pool, prod
}

func signedTx(t *testing.T, k *crypto.PrivateKey, nonce uint64, to common.Address, value *big.Int, gasPrice int64) *types.Transaction {
	t.Helper()
	tx := types.NewTransaction(testChainID, nonce, to, value, types.IntrinsicGas, big.NewInt(gasPrice), nil)
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

// Why quantities are hex strings, not JSON numbers: 10^24 does not fit in a
// float64 without loss, so round-tripping it through a JSON number returns a
// wrong balance.
func TestQuantitySurvivesLargeValues(t *testing.T) {
	huge, _ := new(big.Int).SetString("1000000000000000000000001", 10)
	data, err := json.Marshal(Q(huge))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), `"0x`) {
		t.Fatalf("quantity must encode as a hex string, got %s", data)
	}
	var back Quantity
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Cmp(huge) != 0 {
		t.Fatalf("precision lost: got %s want %s", back.Int, huge)
	}
}

// A bare JSON number must be rejected, not coerced: accepting it reintroduces
// the precision bug above.
func TestQuantityRejectsJSONNumber(t *testing.T) {
	var q Quantity
	if err := json.Unmarshal([]byte("12345"), &q); err == nil {
		t.Fatal("a JSON number was accepted as a quantity")
	}
}

func TestBalanceAndNonceOverRPC(t *testing.T) {
	alice := newKey(t)
	_, c, _, _, _ := setup(t, alice.Address())

	var bal Quantity
	if err := c.Call("chain_getBalance", &bal, alice.Address()); err != nil {
		t.Fatal(err)
	}
	want, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	if bal.Cmp(want) != 0 {
		t.Fatalf("balance: got %s want %s", bal.Int, want)
	}

	var nonce Quantity
	if err := c.Call("chain_getNonce", &nonce, alice.Address()); err != nil {
		t.Fatal(err)
	}
	if nonce.U64() != 0 {
		t.Fatalf("nonce: got %d want 0", nonce.U64())
	}
}

func TestSendTransactionAndMine(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	_, c, bc, _, prod := setup(t, alice.Address())

	tx := signedTx(t, alice, 0, bob.Address(), big.NewInt(50_000), 3)

	var hash common.Hash
	if err := c.Call("chain_sendTransaction", &hash, FromTx(tx)); err != nil {
		t.Fatal(err)
	}
	if hash != tx.Hash() {
		t.Fatal("node returned a different tx hash than the client computed")
	}

	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}

	var bal Quantity
	if err := c.Call("chain_getBalance", &bal, bob.Address()); err != nil {
		t.Fatal(err)
	}
	if bal.Int64() != 50_000 {
		t.Fatalf("bob balance: got %s want 50000", bal.Int)
	}
	if bc.Head().Height() != 1 {
		t.Fatal("chain did not advance")
	}
}

// A pending transaction has no receipt, and the node must say so. Inventing one
// would make a wallet report "payment confirmed" for a tx still in the mempool
// that may never be mined.
func TestPendingTxHasNoReceipt(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	_, c, _, _, prod := setup(t, alice.Address())

	tx := signedTx(t, alice, 0, bob.Address(), big.NewInt(1), 1)
	var hash common.Hash
	if err := c.Call("chain_sendTransaction", &hash, FromTx(tx)); err != nil {
		t.Fatal(err)
	}

	var r ReceiptResult
	if err := c.Call("chain_getTransactionReceipt", &r, hash); err != ErrNullResult {
		t.Fatalf("pending tx returned a receipt: %v", err)
	}

	// But the tx itself is visible, with null block fields: how a client tells
	// pending from unknown.
	var tr TxResult
	if err := c.Call("chain_getTransactionByHash", &tr, hash); err != nil {
		t.Fatal(err)
	}
	if tr.BlockHash != nil {
		t.Fatal("pending tx reported a block hash")
	}
	if tr.From != alice.Address() {
		t.Fatal("from was not recovered correctly")
	}

	// After mining, the receipt appears.
	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := c.Call("chain_getTransactionReceipt", &r, hash); err != nil {
		t.Fatal(err)
	}
	if r.Status.U64() != types.ReceiptSuccess {
		t.Fatalf("status: got %d want 1", r.Status.U64())
	}
	if r.BlockHeight.U64() != 1 {
		t.Fatalf("block height: got %d want 1", r.BlockHeight.U64())
	}
}

func TestUnknownThingsReturnNullNotError(t *testing.T) {
	_, c, _, _, _ := setup(t)

	var b BlockResult
	if err := c.Call("chain_getBlockByNumber", &b, QU(9999), false); err != ErrNullResult {
		t.Fatalf("missing block: got %v want null", err)
	}
	var r ReceiptResult
	if err := c.Call("chain_getTransactionReceipt", &r, common.Hash{0x01}); err != ErrNullResult {
		t.Fatalf("missing receipt: got %v want null", err)
	}
}

// The node must not accept a tx signed for a different chain, even from a caller
// trusted enough to reach the port.
func TestRPCRejectsWrongChainID(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	_, c, _, _, _ := setup(t, alice.Address())

	tx := types.NewTransaction(999, 0, bob.Address(), big.NewInt(1), types.IntrinsicGas, big.NewInt(1), nil)
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	var hash common.Hash
	if err := c.Call("chain_sendTransaction", &hash, FromTx(tx)); err == nil {
		t.Fatal("tx for another chain was accepted")
	}
}

// Everything the RPC returns about a tx is derived from the signature; a client
// cannot inject a `from`, because there is no such field to set.
func TestFromIsRecoveredNotClaimed(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	_, c, _, _, _ := setup(t, alice.Address())

	tx := signedTx(t, alice, 0, bob.Address(), big.NewInt(1), 1)
	args := FromTx(tx)

	raw, _ := json.Marshal(args)
	var asMap map[string]interface{}
	json.Unmarshal(raw, &asMap)
	if _, present := asMap["from"]; present {
		t.Fatal("the wire format has a `from` field — it must not")
	}

	var hash common.Hash
	if err := c.Call("chain_sendTransaction", &hash, args); err != nil {
		t.Fatal(err)
	}
	var tr TxResult
	if err := c.Call("chain_getTransactionByHash", &tr, hash); err != nil {
		t.Fatal(err)
	}
	if tr.From != alice.Address() {
		t.Fatalf("from: got %s want %s", tr.From, alice.Address())
	}
}

func TestMethodNotFound(t *testing.T) {
	_, c, _, _, _ := setup(t)
	err := c.Call("chain_stealAllTheMoney", nil)
	if err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("got %v, want method-not-found", err)
	}
}

func TestBatchRequest(t *testing.T) {
	alice := newKey(t)
	ts, _, _, _, _ := setup(t, alice.Address())

	body := `[
      {"jsonrpc":"2.0","method":"chain_chainId","id":1},
      {"jsonrpc":"2.0","method":"chain_blockNumber","id":2}
    ]`
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("batch: got %d responses want 2", len(out))
	}
	if out[0]["result"] != "0x539" { // 1337
		t.Fatalf("chain id: got %v want 0x539", out[0]["result"])
	}
}

// An unbounded request body is a free OOM. The limit must actually bite.
func TestOversizedBodyRejected(t *testing.T) {
	ts, _, _, _, _ := setup(t)
	huge := strings.Repeat("a", (1<<20)+1024)
	body := `{"jsonrpc":"2.0","method":"chain_chainId","params":["` + huge + `"],"id":1}`
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] == nil {
		t.Fatal("oversized body was accepted")
	}
}

func TestGetBlockFullVsHashes(t *testing.T) {
	alice, bob := newKey(t), newKey(t)
	_, c, _, pool, prod := setup(t, alice.Address())

	tx := signedTx(t, alice, 0, bob.Address(), big.NewInt(7), 1)
	if err := pool.Add(tx, testChainID); err != nil {
		t.Fatal(err)
	}
	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}

	var hashesOnly struct {
		Txs []common.Hash `json:"txs"`
	}
	if err := c.Call("chain_getBlockByNumber", &hashesOnly, QU(1), false); err != nil {
		t.Fatal(err)
	}
	if len(hashesOnly.Txs) != 1 || hashesOnly.Txs[0] != tx.Hash() {
		t.Fatal("hash-only block body is wrong")
	}

	var full struct {
		Txs []TxResult `json:"txs"`
	}
	if err := c.Call("chain_getBlockByNumber", &full, QU(1), true); err != nil {
		t.Fatal(err)
	}
	if len(full.Txs) != 1 || full.Txs[0].From != alice.Address() {
		t.Fatal("full block body is wrong")
	}
}
