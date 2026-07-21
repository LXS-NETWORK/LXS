package vm

import "testing"

// staticCallTo emits a STATICCALL to `to` forwarding all gas, no args, no return
// window. STATICCALL takes no value word, so its stack is gas, addr, argsOff,
// argsSize, retOff, retSize.
func staticCallTo(to byte) []byte {
	return b(
		byte(PUSH1), 0x00, // retSize
		byte(PUSH1), 0x00, // retOff
		byte(PUSH1), 0x00, // argsSize
		byte(PUSH1), 0x00, // argsOff
		byte(PUSH1), to, // target
		byte(GAS),
		byte(STATICCALL),
	)
}

// staticFlag STATICCALLs `to` and returns the 0/1 success flag as a word.
func staticFlag(to byte) []byte {
	return append(staticCallTo(to),
		byte(PUSH1), 0x00, byte(MSTORE), byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))
}

// TestStaticCallReads: a STATICCALL to a read-only callee succeeds and its
// output is readable via RETURNDATACOPY (the path for a `view` call).
func TestStaticCallReads(t *testing.T) {
	state := newMockState()
	callee := addr(0xBB)
	// Callee returns the word 0x42.
	state.SetCode(callee, b(
		byte(PUSH1), 0x42, byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	))
	code := append(staticCallTo(0xBB),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(RETURNDATACOPY),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))

	r := Run(code, nil, 1_000_000, Context{Address: addr(0xAA), State: state})
	if r.Err != nil {
		t.Fatalf("static call to a read-only callee failed: %v", r.Err)
	}
	if len(r.Ret) != 32 || r.Ret[31] != 0x42 {
		t.Fatalf("STATICCALL returned %x, want a word ending in 42", r.Ret)
	}
}

// TestStaticCallForbidsSStore: a STATICCALL whose callee tries to SSTORE fails
// (returns 0) and leaves no write behind, the guarantee a `view` function relies
// on.
func TestStaticCallForbidsSStore(t *testing.T) {
	state := newMockState()
	writer := addr(0xCC)
	state.SetCode(writer, b(byte(PUSH1), 0x01, byte(PUSH1), 0x00, byte(SSTORE), byte(STOP)))

	r := Run(staticFlag(0xCC), nil, 1_000_000, Context{Address: addr(0xAA), State: state})
	if r.Err != nil {
		t.Fatalf("caller must survive the callee's static fault: %v", r.Err)
	}
	if r.Ret[31] != 0 {
		t.Fatalf("STATICCALL to an SSTORE-ing callee returned success %d, want 0", r.Ret[31])
	}
	if !state.GetStorage(writer, slot(0)).IsZero() {
		t.Fatal("a STATICCALL let a write through — read-only was not enforced")
	}
}

// TestStaticCallForbidsLog: emitting an event is also a state change and must
// fault inside a static frame.
func TestStaticCallForbidsLog(t *testing.T) {
	state := newMockState()
	logger := addr(0xDD)
	state.SetCode(logger, b(byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(LOG0), byte(STOP)))

	r := Run(staticFlag(0xDD), nil, 1_000_000, Context{Address: addr(0xAA), State: state})
	if r.Err != nil {
		t.Fatalf("caller must survive: %v", r.Err)
	}
	if r.Ret[31] != 0 {
		t.Fatalf("STATICCALL to a LOGging callee returned %d, want 0", r.Ret[31])
	}
	if len(state.logs) != 0 {
		t.Fatalf("a STATICCALL emitted %d logs, want 0", len(state.logs))
	}
}

// TestStaticPropagatesThroughNestedCall: read-only cannot be escaped by nesting.
// A STATICCALL into A, where A makes a plain CALL into B that SSTOREs, must still
// leave B's storage untouched.
func TestStaticPropagatesThroughNestedCall(t *testing.T) {
	state := newMockState()
	a := addr(0xBB)
	bStore := addr(0xCC)
	// B writes slot 0.
	state.SetCode(bStore, b(byte(PUSH1), 0x07, byte(PUSH1), 0x00, byte(SSTORE), byte(STOP)))
	// A makes a plain CALL to B, then STOPs.
	state.SetCode(a, append(callTo(0x00, 0xCC), byte(STOP)))

	r := Run(staticFlag(0xBB), nil, 1_000_000, Context{Address: addr(0xAA), State: state})
	if r.Err != nil {
		t.Fatalf("caller must survive: %v", r.Err)
	}
	if !state.GetStorage(bStore, slot(0)).IsZero() {
		t.Fatal("a nested CALL from a static frame let a write through — static did not propagate")
	}
}
