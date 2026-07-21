package rpc

import (
	"encoding/json"
	"strings"
	"testing"
)

// JSON-RPC 2.0: a success response must always contain "result" (even null); an error
// response must contain "error" and no "result". Strict clients (MetaMask polling) depend on it.
func TestResponseAlwaysHasResultOnSuccess(t *testing.T) {
	ok := response{JSONRPC: "2.0", Result: nil, ID: json.RawMessage("1")}
	b, err := json.Marshal(ok)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"result":null`) {
		t.Fatalf("a null-result success must serialize with result:null, got %s", b)
	}
	if strings.Contains(string(b), `"error"`) {
		t.Fatalf("a success must not carry error, got %s", b)
	}

	bad := response{JSONRPC: "2.0", Error: &rpcError{Code: -1, Message: "x"}, ID: json.RawMessage("1")}
	b2, _ := json.Marshal(bad)
	if strings.Contains(string(b2), `"result"`) {
		t.Fatalf("an error response must not carry result, got %s", b2)
	}
	if !strings.Contains(string(b2), `"error"`) {
		t.Fatalf("an error response must carry error, got %s", b2)
	}
}
