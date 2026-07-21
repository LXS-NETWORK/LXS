package vm

import (
	"math/big"
	"testing"

	"lxs/common"
)

// addr builds an address whose low byte is b, the byte a `PUSH1 b` puts on the
// stack. Using only the low byte keeps the test bytecode to a single PUSH1.
func addr(b byte) common.Address {
	var a common.Address
	a[len(a)-1] = b
	return a
}

// slot builds the storage key for slot n the same way SSTORE does, so a test
// reads back exactly what the opcode wrote.
func slot(n int64) common.Hash { return wordToHash(big.NewInt(n)) }

// lowByte is the last byte of a stored word — enough to check a small value
// like 42 or 99 without constructing a full 32-byte hash.
func lowByte(h common.Hash) byte { return h[len(h)-1] }

// callTo emits the CALL setup + CALL for a value-bearing call to `to` with no
// args and no return window, forwarding all gas (the GAS opcode) so the callee
// can afford an SSTORE. Operands are pushed in reverse of the pop order.
func callTo(value, to byte) []byte {
	return b(
		byte(PUSH1), 0x00, // retSize
		byte(PUSH1), 0x00, // retOff
		byte(PUSH1), 0x00, // argsSize
		byte(PUSH1), 0x00, // argsOff
		byte(PUSH1), value, // value
		byte(PUSH1), to, // target address
		byte(GAS), // forward all remaining gas
		byte(CALL),
	)
}

// TestCrossContractStorageIsolation: a Token contract CALLs a Registry
// contract, and each contract's slot 0 must hold its own value afterwards. CALL
// runs the callee in the callee's storage, never the caller's.
func TestCrossContractStorageIsolation(t *testing.T) {
	state := newMockState()
	tokenAddr := addr(0xAA)
	registryAddr := addr(0xBB)

	// Registry: store 99 into its own slot 0, then stop.
	state.SetCode(registryAddr, b(
		byte(PUSH1), 0x63, byte(PUSH1), 0x00, byte(SSTORE), byte(STOP),
	))

	// Token: store 42 into its own slot 0, then CALL the registry.
	tokenCode := append(
		b(byte(PUSH1), 0x2a, byte(PUSH1), 0x00, byte(SSTORE)),
		callTo(0x00, 0xBB)...,
	)
	tokenCode = append(tokenCode, byte(STOP))

	ctx := Context{Address: tokenAddr, State: state}
	r := Run(tokenCode, nil, 1_000_000, ctx)
	if r.Err != nil {
		t.Fatalf("token execution failed: %v", r.Err)
	}

	if got := lowByte(state.GetStorage(tokenAddr, slot(0))); got != 0x2a {
		t.Fatalf("token slot 0 = %d, want 42 — the registry's write leaked into the caller's storage", got)
	}
	if got := lowByte(state.GetStorage(registryAddr, slot(0))); got != 0x63 {
		t.Fatalf("registry slot 0 = %d, want 99 — the callee's write did not land in the callee's storage", got)
	}
}

// TestDelegateCallWritesCallerStorage: DELEGATECALL runs the library's code in
// the caller's storage. The proxy pattern depends on this: the library's SSTORE
// must hit the proxy's slot, not the library's own.
func TestDelegateCallWritesCallerStorage(t *testing.T) {
	state := newMockState()
	proxyAddr := addr(0xAA)
	libAddr := addr(0xCC)

	// Library: store 99 into slot 1.
	state.SetCode(libAddr, b(
		byte(PUSH1), 0x63, byte(PUSH1), 0x01, byte(SSTORE), byte(STOP),
	))

	// Proxy: DELEGATECALL the library (no value word for DELEGATECALL).
	proxyCode := b(
		byte(PUSH1), 0x00, // retSize
		byte(PUSH1), 0x00, // retOff
		byte(PUSH1), 0x00, // argsSize
		byte(PUSH1), 0x00, // argsOff
		byte(PUSH1), 0xCC, // library address
		byte(GAS), // forward all remaining gas
		byte(DELEGATECALL),
		byte(STOP),
	)

	ctx := Context{Address: proxyAddr, State: state}
	if r := Run(proxyCode, nil, 1_000_000, ctx); r.Err != nil {
		t.Fatalf("proxy execution failed: %v", r.Err)
	}

	if got := lowByte(state.GetStorage(proxyAddr, slot(1))); got != 0x63 {
		t.Fatalf("proxy slot 1 = %d, want 99 — DELEGATECALL did not write the CALLER's storage", got)
	}
	if s := state.GetStorage(libAddr, slot(1)); !s.IsZero() {
		t.Fatalf("library slot 1 = %x, want empty — DELEGATECALL wrote the callee's storage instead of the caller's", s)
	}
}

// TestCallTransfersValue: a plain CALL with value moves LXS from caller to
// callee and reports success (1).
func TestCallTransfersValue(t *testing.T) {
	state := newMockState()
	tokenAddr := addr(0xAA)
	recipient := addr(0xDD)
	state.AddBalance(tokenAddr, big.NewInt(1000))
	state.SetCode(recipient, b(byte(STOP))) // accepts the call, does nothing

	// CALL recipient with value 100, then store the success flag into slot 0.
	code := append(callTo(0x64, 0xDD), byte(PUSH1), 0x00, byte(SSTORE), byte(STOP))

	ctx := Context{Address: tokenAddr, State: state}
	if r := Run(code, nil, 1_000_000, ctx); r.Err != nil {
		t.Fatalf("execution failed: %v", r.Err)
	}

	if b := state.GetBalance(tokenAddr); b.Cmp(big.NewInt(900)) != 0 {
		t.Fatalf("caller balance = %s, want 900 — value did not leave the sender", b)
	}
	if b := state.GetBalance(recipient); b.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("recipient balance = %s, want 100 — value did not reach the callee", b)
	}
	if got := lowByte(state.GetStorage(tokenAddr, slot(0))); got != 1 {
		t.Fatalf("CALL success flag = %d, want 1", got)
	}
}

// TestCallRevertReturnsValue: if the callee reverts, the value must return to
// the sender and the caller must see 0 (failure), not 1.
func TestCallRevertReturnsValue(t *testing.T) {
	state := newMockState()
	tokenAddr := addr(0xAA)
	recipient := addr(0xDD)
	state.AddBalance(tokenAddr, big.NewInt(1000))
	// Recipient reverts immediately: refuses the payment.
	state.SetCode(recipient, b(byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(REVERT)))

	code := append(callTo(0x64, 0xDD), byte(PUSH1), 0x00, byte(SSTORE), byte(STOP))

	ctx := Context{Address: tokenAddr, State: state}
	if r := Run(code, nil, 1_000_000, ctx); r.Err != nil {
		t.Fatalf("caller must survive a callee revert, got: %v", r.Err)
	}

	if b := state.GetBalance(tokenAddr); b.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("caller balance = %s, want 1000 — value was not returned after the callee reverted", b)
	}
	if b := state.GetBalance(recipient); b.Sign() != 0 {
		t.Fatalf("recipient balance = %s, want 0 — a reverted call kept the value", b)
	}
	if got := lowByte(state.GetStorage(tokenAddr, slot(0))); got != 0 {
		t.Fatalf("CALL success flag = %d, want 0 — a reverted call reported success", got)
	}
}

// TestCallDepthLimit pins the depth boundary. A frame at MaxCallDepth must have
// its CALL refused (push 0) without executing the callee; a frame one level
// shallower must be allowed. This bounds recursion, it is not a reentrancy guard.
func TestCallDepthLimit(t *testing.T) {
	state := newMockState()
	caller := addr(0xAA)
	target := addr(0xDD)
	state.SetCode(target, b(byte(STOP))) // a callee that would succeed if reached

	// Call the target, then record whether it succeeded into slot 0.
	code := append(callTo(0x00, 0xDD), byte(PUSH1), 0x00, byte(SSTORE), byte(STOP))
	ctx := Context{Address: caller, State: state}

	// One level below the ceiling: the CALL is allowed, callee runs, flag = 1.
	if r := execute(code, nil, 1_000_000, ctx, MaxCallDepth-1); r.Err != nil {
		t.Fatalf("call just below the depth limit failed: %v", r.Err)
	}
	if got := lowByte(state.GetStorage(caller, slot(0))); got != 1 {
		t.Fatalf("at depth %d the call should succeed (flag 1), got %d", MaxCallDepth-1, got)
	}

	// AT the ceiling: the CALL is refused before running, flag = 0.
	if r := execute(code, nil, 1_000_000, ctx, MaxCallDepth); r.Err != nil {
		t.Fatalf("frame at the depth limit must not fault, only refuse the call: %v", r.Err)
	}
	if got := lowByte(state.GetStorage(caller, slot(0))); got != 0 {
		t.Fatalf("at depth %d the call must be refused (flag 0), got %d — the depth ceiling did not hold", MaxCallDepth, got)
	}
}

// TestDeepRecursionTerminates: a contract that CALLs itself forever, forwarding
// all gas, must return cleanly rather than overflow the Go stack. It terminates
// via the depth ceiling or gas exhaustion.
func TestDeepRecursionTerminates(t *testing.T) {
	state := newMockState()
	self := addr(0xEE)
	// Self-call forwarding all remaining gas (GAS opcode), no value.
	recursive := b(
		byte(PUSH1), 0x00, // retSize
		byte(PUSH1), 0x00, // retOff
		byte(PUSH1), 0x00, // argsSize
		byte(PUSH1), 0x00, // argsOff
		byte(PUSH1), 0x00, // value
		byte(PUSH1), 0xEE, // self
		byte(GAS), // forward all remaining gas
		byte(CALL),
		byte(STOP),
	)
	state.SetCode(self, recursive)

	ctx := Context{Address: self, State: state}
	// A large budget so recursion goes deep before gas or the ceiling stops it.
	r := Run(recursive, nil, 100_000_000, ctx)
	if r.Err != nil {
		t.Fatalf("runaway recursion must terminate cleanly, got: %v", r.Err)
	}
}
