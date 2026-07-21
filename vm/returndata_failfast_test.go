package vm

import "testing"

// A CALL that is refused without executing (here: value-bearing with an
// insufficient balance) must clear RETURNDATA. Otherwise the previous sub-call's
// output survives, and the standard OpenZeppelin bubble-up
// (`if (!success && returndatasize()!=0) revert(returndata)`) reverts with stale
// garbage — and two nodes hitting it diverge. geth resets returndata on every
// refused CALL/CREATE.
func TestReturnDataClearedOnFailFastCall(t *testing.T) {
	state := newMockState()
	caller := addr(0xAA) // balance 0 in a fresh mock state
	callee := addr(0xBB)

	// Callee returns a 32-byte word, so after calling it RETURNDATASIZE is 32.
	state.SetCode(callee, b(
		byte(PUSH1), 0x42, byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	))

	code := callTo(0x00, 0xBB) // success -> returndata = 32 bytes
	code = append(code, byte(POP))
	// Value-bearing CALL from a 0-balance account -> fail-fast (pushes 0, never executes).
	code = append(code, callTo(0x01, 0xCC)...)
	code = append(code, byte(POP))
	// RETURNDATASIZE must now be 0.
	code = append(code,
		byte(RETURNDATASIZE), byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))

	r := Run(code, nil, 1_000_000, Context{Address: caller, State: state})
	if r.Err != nil {
		t.Fatalf("execution failed: %v", r.Err)
	}
	if len(r.Ret) != 32 {
		t.Fatalf("expected a 32-byte word, got %x", r.Ret)
	}
	for _, bb := range r.Ret {
		if bb != 0 {
			t.Fatalf("RETURNDATASIZE after a fail-fast CALL = %x, want 0 (stale buffer leaked)", r.Ret)
		}
	}
}
