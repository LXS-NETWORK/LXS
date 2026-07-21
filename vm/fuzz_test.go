package vm

import (
	"bytes"
	"math/rand"
	"testing"
)

// Fuzz the interpreter with garbage. The VM's safety claim is bounded
// execution: any byte sequence, on any input, must halt without crashing the
// host and do so deterministically (else consensus splits). These tests check
// that no program can break the machine, not that any program is correct.

// fuzzAlphabet is the set of implemented opcodes, so random programs spend most
// of their bytes on real instructions rather than a wall of INVALID. Raw random
// bytes are still mixed in (see randProgram) to exercise the decoder on garbage.
var fuzzAlphabet = []byte{
	byte(STOP), byte(ADD), byte(MUL), byte(SUB), byte(DIV), byte(SDIV),
	byte(MOD), byte(SMOD), byte(ADDMOD), byte(MULMOD), byte(EXP), byte(SIGNEXTEND),
	byte(LT), byte(GT), byte(EQ), byte(ISZERO), byte(AND), byte(OR), byte(XOR),
	byte(NOT), byte(BYTE), byte(SHL), byte(SHR), byte(SAR), byte(SHA3),
	byte(ADDRESS), byte(CALLER), byte(CALLVALUE), byte(CALLDATALOAD),
	byte(CALLDATASIZE), byte(CALLDATACOPY), byte(CODESIZE), byte(CODECOPY),
	byte(RETURNDATASIZE), byte(RETURNDATACOPY), byte(GASLIMIT),
	byte(POP), byte(MLOAD), byte(MSTORE), byte(MSTORE8), byte(SLOAD), byte(SSTORE), byte(PUSH0), byte(MCOPY),
	byte(JUMP), byte(JUMPI), byte(PC), byte(MSIZE), byte(GAS), byte(JUMPDEST),
	byte(PUSH1), byte(PUSH1) + 1, byte(PUSH32), byte(DUP1), byte(SWAP1),
	byte(LOG0), byte(LOG0) + 3, byte(CALL), byte(DELEGATECALL),
	byte(RETURN), byte(REVERT), byte(INVALID),
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}

// randProgram builds a program of roughly n bytes, mostly from real opcodes,
// filling in PUSH data so a PUSH is not perpetually truncated by the next byte.
func randProgram(r *rand.Rand, n int) []byte {
	code := make([]byte, 0, n+32)
	for len(code) < n {
		if r.Intn(4) == 0 {
			code = append(code, byte(r.Intn(256))) // garbage byte
			continue
		}
		op := fuzzAlphabet[r.Intn(len(fuzzAlphabet))]
		code = append(code, op)
		if op >= byte(PUSH1) && op <= byte(PUSH32) {
			code = append(code, randBytes(r, int(op-byte(PUSH1))+1)...)
		}
	}
	return code
}

// runNoPanic runs the VM and reports whether it panicked. A panic is a host
// crash, the one outcome bounded execution must never have.
func runNoPanic(code, input []byte, gas uint64, ctx Context) (res Result, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	return Run(code, input, gas, ctx), false
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

// TestVMBoundedAndDeterministic runs tens of thousands of random programs from
// a fixed seed and asserts three invariants: no panic, gas never grows, and two
// identical runs produce byte-identical results.
func TestVMBoundedAndDeterministic(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	const rounds = 30000
	for i := 0; i < rounds; i++ {
		code := randProgram(r, 1+r.Intn(80))
		var input []byte
		if r.Intn(2) == 0 {
			input = randBytes(r, r.Intn(72))
		}
		gas := uint64(1 + r.Intn(300_000))

		res1, p1 := runNoPanic(code, input, gas, Context{Address: addr(0x11), State: newMockState()})
		if p1 {
			t.Fatalf("VM PANICKED (round %d)\n code=%x\n input=%x\n gas=%d", i, code, input, gas)
		}
		if res1.GasLeft > gas {
			t.Fatalf("gas GREW: left %d > in %d\n code=%x", res1.GasLeft, gas, code)
		}

		// Determinism is consensus-critical: same code + input + fresh state must
		// yield the same (return data, gas, error) every time.
		res2, p2 := runNoPanic(code, input, gas, Context{Address: addr(0x11), State: newMockState()})
		if p2 || res1.GasLeft != res2.GasLeft || !bytes.Equal(res1.Ret, res2.Ret) || errString(res1.Err) != errString(res2.Err) {
			t.Fatalf("NON-DETERMINISTIC execution (round %d)\n code=%x\n r1=%+v\n r2=%+v", i, code, res1, res2)
		}
	}
}

// TestMemoryZeroSizeHugeOffsetDoesNotAllocate pins a crash fuzzing found: a
// memory op with size 0 and an absurd offset must be a free no-op, not an
// attempt to allocate `offset` bytes. Regression guard for the chargeMem fix.
func TestMemoryZeroSizeHugeOffsetDoesNotAllocate(t *testing.T) {
	maxWord := bytes.Repeat([]byte{0xff}, 32) // offset = 2^256-1

	// LOG0 with size 0, offset = 2^256-1. opLog pops off (top) then size, so
	// push size 0 first, then the offset.
	logCode := append([]byte{byte(PUSH1), 0x00, byte(PUSH32)}, maxWord...)
	logCode = append(logCode, byte(LOG0), byte(STOP))
	if r := Run(logCode, nil, 100_000, Context{Address: addr(0x11), State: newMockState()}); r.Err != nil {
		t.Fatalf("LOG0 with size 0 and a huge offset should succeed, got %v", r.Err)
	}

	// RETURN with size 0, huge offset (the readMem path) must return empty, not
	// allocate. RETURN pops off then size, so push size 0 then offset.
	retCode := append([]byte{byte(PUSH1), 0x00, byte(PUSH32)}, maxWord...)
	retCode = append(retCode, byte(RETURN))
	if r := Run(retCode, nil, 100_000, Context{}); r.Err != nil || len(r.Ret) != 0 {
		t.Fatalf("RETURN with size 0 should give empty output, got ret=%x err=%v", r.Ret, r.Err)
	}
}

// FuzzInterpreter drives the same invariants under `go test -fuzz` with
// coverage-guided inputs. The seed corpus doubles as a smoke check when fuzzing
// is not requested.
func FuzzInterpreter(f *testing.F) {
	f.Add([]byte{0x60, 0x01, 0x60, 0x02, 0x01, 0x00}, []byte{}, uint64(100_000)) // 1+2;STOP
	f.Add([]byte{0x5b, 0x60, 0x00, 0x56}, []byte{}, uint64(5_000))               // infinite loop
	f.Add([]byte{0x60, 0x00, 0x60, 0x00, 0x20}, []byte{}, uint64(100_000))       // SHA3
	f.Add([]byte{0x36, 0x60, 0x00, 0x35}, []byte{0xaa, 0xbb}, uint64(100_000))   // calldata
	f.Add([]byte{0xf1}, []byte{}, uint64(100_000))                               // CALL with empty stack
	// The zero-size-huge-offset crash fuzzing found, kept as a permanent seed.
	f.Add([]byte{0x59, 0x67, 0x09, 0xf3, 0x30, 0x1a, 0x51, 0x1b, 0x11, 0x02, 0xa0}, []byte{}, uint64(230_479))

	f.Fuzz(func(t *testing.T, code, input []byte, gas uint64) {
		if gas > 5_000_000 {
			gas = 5_000_000 // keep each input fast; invariant holds at any gas
		}
		res, panicked := runNoPanic(code, input, gas, Context{Address: addr(0x11), State: newMockState()})
		if panicked {
			t.Fatalf("VM panicked on code=%x input=%x gas=%d", code, input, gas)
		}
		if res.GasLeft > gas {
			t.Fatalf("gas grew: left %d > in %d", res.GasLeft, gas)
		}
	})
}
