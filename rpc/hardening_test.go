package rpc

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// TestCappedCallGasNeverExceedsCeiling: a read-only call may ask for less gas
// than the ceiling, but a request for more (or an absurd one) is clamped. This
// is the fix for the free-compute DoS where args.Gas overrode the cap entirely.
func TestCappedCallGasNeverExceedsCeiling(t *testing.T) {
	if got := cappedCallGas(nil); got != callGasCap {
		t.Fatalf("absent gas: got %d, want the cap %d", got, callGasCap)
	}
	small := QU(100)
	if got := cappedCallGas(&small); got != 100 {
		t.Fatalf("small request: got %d, want 100 (honoured, it is below the cap)", got)
	}
	over := QU(callGasCap + 1_000_000)
	if got := cappedCallGas(&over); got != callGasCap {
		t.Fatalf("over-cap request: got %d, want it clamped to %d", got, callGasCap)
	}
	huge := QU(math.MaxUint64)
	if got := cappedCallGas(&huge); got != callGasCap {
		t.Fatalf("MaxUint64 request: got %d, want it clamped to %d (this is the DoS)", got, callGasCap)
	}
}

// TestPanicInHandlerDoesNotCrashNode: a panicking handler must be caught and
// turned into a generic internal error, never a crash, and the server must keep
// serving.
func TestPanicInHandlerDoesNotCrashNode(t *testing.T) {
	s := NewServer()
	s.Register("boom", func(json.RawMessage) (interface{}, error) { panic("secret internal detail") })
	s.Register("ok", func(json.RawMessage) (interface{}, error) { return "fine", nil })

	res := s.dispatch(request{JSONRPC: "2.0", Method: "boom", ID: json.RawMessage("1")})
	if res.Error == nil {
		t.Fatal("a panicking handler must produce an error response, not crash")
	}
	if res.Error.Code != CodeInternalError {
		t.Fatalf("panic error code = %d, want CodeInternalError %d", res.Error.Code, CodeInternalError)
	}
	// The panic value must not leak to the caller (a fingerprinting aid).
	if strings.Contains(res.Error.Message, "secret internal detail") {
		t.Fatalf("panic detail leaked to client: %q", res.Error.Message)
	}

	// The server survives and still serves other methods.
	res2 := s.dispatch(request{JSONRPC: "2.0", Method: "ok", ID: json.RawMessage("2")})
	if res2.Error != nil || res2.Result != "fine" {
		t.Fatalf("server did not recover: err=%v result=%v", res2.Error, res2.Result)
	}
}
