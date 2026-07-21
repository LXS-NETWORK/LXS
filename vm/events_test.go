package vm

import (
	"bytes"
	"math/big"
	"testing"
)

// TestLogEmission checks that LOG1 records an event with the running contract's
// address, the topic from the stack, and the data window from memory.
func TestLogEmission(t *testing.T) {
	state := newMockState()
	self := addr(0xAA)
	topic := new(big.Int).SetInt64(0xdeadbeef) // stands in for keccak(event sig)
	data := new(big.Int).SetInt64(0x42)

	// mem[0] = data; then LOG1(off=0, size=32, topic).
	code := append(push32(data), byte(PUSH1), 0x00, byte(MSTORE))
	code = append(code, push32(topic)...)         // topic0 (bottom)
	code = append(code, byte(PUSH1), 0x20)        // size
	code = append(code, byte(PUSH1), 0x00)        // offset (top)
	code = append(code, byte(LOG0+1), byte(STOP)) // LOG1

	ctx := Context{Address: self, State: state}
	if r := Run(code, nil, 1_000_000, ctx); r.Err != nil {
		t.Fatalf("execution failed: %v", r.Err)
	}

	if len(state.logs) != 1 {
		t.Fatalf("emitted %d logs, want 1", len(state.logs))
	}
	log := state.logs[0]
	if log.Address != self {
		t.Errorf("log address = %x, want the running contract %x", log.Address, self)
	}
	if len(log.Topics) != 1 || log.Topics[0] != wordToHash(topic) {
		t.Errorf("log topics = %x, want [%x]", log.Topics, wordToHash(topic))
	}
	var wantData [32]byte
	data.FillBytes(wantData[:])
	if !bytes.Equal(log.Data, wantData[:]) {
		t.Errorf("log data = %x, want %x", log.Data, wantData)
	}
}

// TestLogGasCost pins the LOG price: base + per-topic + per-byte, on top of the
// memory the data window touches.
func TestLogGasCost(t *testing.T) {
	state := newMockState()
	// LOG2 over a 32-byte window: base(375) + 2*375 + 8*32(=256) + memory for
	// one word(3) + two PUSH1 topics(6) + PUSH1 size/off(6) = 1402.
	code := b(
		byte(PUSH1), 0xbb, // topic1
		byte(PUSH1), 0xaa, // topic0
		byte(PUSH1), 0x20, // size 32
		byte(PUSH1), 0x00, // offset 0
		byte(LOG0+2),
		byte(STOP),
	)
	const start = 100_000
	r := Run(code, nil, start, Context{Address: addr(1), State: state})
	if r.Err != nil {
		t.Fatalf("execution failed: %v", r.Err)
	}
	want := uint64(LogGas + 2*LogTopicGas + 8*32 + 3 + 6 + 6)
	if used := start - r.GasLeft; used != want {
		t.Fatalf("LOG2 used %d gas, want %d", used, want)
	}
}

// TestLogRevertedDisappears: an event emitted by a call that then reverts must
// not survive. A caller CALLs an emitter that logs and reverts; afterwards
// there must be no log, since the reverted frame's log went with its state.
func TestLogRevertedDisappears(t *testing.T) {
	state := newMockState()
	caller := addr(0xAA)
	emitter := addr(0xBB)
	// Emitter: LOG0 (empty), then REVERT.
	state.SetCode(emitter, b(
		byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(LOG0),
		byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(REVERT),
	))

	code := append(callTo(0x00, 0xBB), byte(STOP))
	if r := Run(code, nil, 1_000_000, Context{Address: caller, State: state}); r.Err != nil {
		t.Fatalf("caller must survive callee revert: %v", r.Err)
	}
	if len(state.logs) != 0 {
		t.Fatalf("a reverted call left %d logs behind, want 0", len(state.logs))
	}
}

// TestReturnData checks RETURNDATASIZE / RETURNDATACOPY expose the last call's
// output, and that an out-of-range copy faults rather than reading zeros.
func TestReturnData(t *testing.T) {
	state := newMockState()
	caller := addr(0xAA)
	callee := addr(0xBB)
	// Callee returns the 32-byte word 0x42.
	state.SetCode(callee, b(
		byte(PUSH1), 0x42, byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	))

	// Caller: CALL callee, then RETURNDATACOPY(mem 0, data 0, size 32), return it.
	inBounds := append(callTo(0x00, 0xBB),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(RETURNDATACOPY),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))
	r := Run(inBounds, nil, 1_000_000, Context{Address: caller, State: state})
	if r.Err != nil {
		t.Fatalf("execution failed: %v", r.Err)
	}
	if len(r.Ret) != 32 || r.Ret[31] != 0x42 {
		t.Fatalf("RETURNDATACOPY produced %x, want a word ending in 42", r.Ret)
	}

	// RETURNDATASIZE must report 32.
	sizeProbe := append(callTo(0x00, 0xBB),
		byte(RETURNDATASIZE), byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))
	rs := Run(sizeProbe, nil, 1_000_000, Context{Address: caller, State: state})
	if rs.Err != nil || len(rs.Ret) != 32 || rs.Ret[31] != 32 {
		t.Fatalf("RETURNDATASIZE = %x (err %v), want 32", rs.Ret, rs.Err)
	}

	// Copying 64 bytes when only 32 were returned must fault, not zero-pad.
	oob := append(callTo(0x00, 0xBB),
		byte(PUSH1), 0x40, byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(RETURNDATACOPY),
		byte(STOP))
	if ro := Run(oob, nil, 1_000_000, Context{Address: caller, State: state}); ro.Err != ErrReturnDataOutOfBounds {
		t.Fatalf("out-of-range RETURNDATACOPY: got %v, want ErrReturnDataOutOfBounds", ro.Err)
	}
}

// TestCodeCopyAndSize covers the two self-inspection opcodes a constructor
// needs. CODECOPY reading past the end of code zero-pads (like calldatacopy),
// and CODESIZE reports the running code's length.
func TestCodeCopyAndSize(t *testing.T) {
	// CODECOPY code[0:32] -> mem[0], RETURN it. Code is <32 bytes, so the tail
	// zero-pads; the head must be the code's own opening bytes.
	code := b(
		byte(PUSH1), 0x20, // length 32
		byte(PUSH1), 0x00, // codeOff 0
		byte(PUSH1), 0x00, // destOff 0
		byte(CODECOPY),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	)
	r := Run(code, nil, 100_000, Context{})
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	want := make([]byte, 32)
	copy(want, code)
	if !bytes.Equal(r.Ret, want) {
		t.Fatalf("CODECOPY returned %x, want the code's own bytes %x", r.Ret, want)
	}

	// CODESIZE of a 9-byte program is 9.
	code2 := b(byte(CODESIZE), byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))
	r2 := Run(code2, nil, 100_000, Context{})
	if r2.Err != nil || len(r2.Ret) != 32 || r2.Ret[31] != byte(len(code2)) {
		t.Fatalf("CODESIZE = %x (err %v), want %d", r2.Ret, r2.Err, len(code2))
	}
}
