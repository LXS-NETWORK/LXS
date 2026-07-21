package rpc

import (
	"encoding/json"
	"testing"

	"lxs/common"
	"lxs/contracts"
)

func logParams(t *testing.T, filter map[string]interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal([]interface{}{filter})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEthGetLogsFiltersByAddressAndTopic(t *testing.T) {
	dev := newKey(t)
	_, c, bc, pool, prod := setup(t, dev.Address())
	api := NewAPI(bc, pool)

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
	if err := c.Call("chain_getTransactionReceipt", &dr, dh); err != nil || dr.ContractAddress == nil {
		t.Fatalf("deploy receipt: %v", err)
	}
	token := *dr.ContractAddress

	bob := newKey(t).Address()
	transfer := deployOrCallTx(t, dev, 1, &token, contracts.TransferCalldata(bob, common.LXS(250_000)))
	var th common.Hash
	if err := c.Call("chain_sendTransaction", &th, FromTx(transfer)); err != nil {
		t.Fatal(err)
	}
	if _, err := prod.Seal(); err != nil {
		t.Fatal(err)
	}

	sig := contracts.TransferEventTopic()

	// Match on address + the Transfer topic across the whole chain.
	res, err := api.EthGetLogs(logParams(t, map[string]interface{}{
		"fromBlock": "earliest", "toBlock": "latest",
		"address": token,
		"topics":  []interface{}{sig},
	}))
	if err != nil {
		t.Fatal(err)
	}
	logs := res.([]interface{})
	if len(logs) != 1 {
		t.Fatalf("got %d logs, want exactly the one Transfer", len(logs))
	}
	m := logs[0].(map[string]interface{})
	if m["address"].(common.Address) != token {
		t.Fatalf("log address = %v, want token %v", m["address"], token)
	}
	if tp := m["topics"].([]common.Hash); len(tp) != 3 || tp[0] != sig {
		t.Fatalf("topics = %v, want [Transfer, from, to]", tp)
	}

	// Wrong address → nothing.
	res, _ = api.EthGetLogs(logParams(t, map[string]interface{}{
		"fromBlock": "earliest", "toBlock": "latest",
		"address": newKey(t).Address(),
	}))
	if n := len(res.([]interface{})); n != 0 {
		t.Fatalf("address mismatch returned %d logs, want 0", n)
	}

	// Wrong topic → nothing.
	res, _ = api.EthGetLogs(logParams(t, map[string]interface{}{
		"fromBlock": "earliest", "toBlock": "latest",
		"address": token,
		"topics":  []interface{}{common.Hash{0x99}},
	}))
	if n := len(res.([]interface{})); n != 0 {
		t.Fatalf("topic mismatch returned %d logs, want 0", n)
	}
}

func TestEthGetLogsRejectsWideRange(t *testing.T) {
	dev := newKey(t)
	_, _, bc, pool, _ := setup(t, dev.Address())
	api := NewAPI(bc, pool)

	// fromBlock 0, toBlock past the cap → refused (DoS guard), not a full scan.
	_, err := api.EthGetLogs(logParams(t, map[string]interface{}{
		"fromBlock": "0x0", "toBlock": "0x2711", // 10001 > maxLogRange
	}))
	if err == nil {
		t.Fatal("a block range wider than maxLogRange must be rejected")
	}
}
