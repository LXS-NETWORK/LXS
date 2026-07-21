package vm

import (
	"errors"
	"testing"
)

// b assembles readable bytecode from opcodes and raw data bytes.
func b(items ...byte) []byte { return items }

// Smoke test: 2 + 3, stored to memory, returned as a 32-byte word. Exercises
// the stack, arithmetic, memory, and RETURN.
func TestAddAndReturn(t *testing.T) {
	code := b(
		byte(PUSH1), 0x03,
		byte(PUSH1), 0x02,
		byte(ADD),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	)
	r := Run(code, nil, 100_000, Context{})
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if len(r.Ret) != 32 || r.Ret[31] != 5 {
		t.Fatalf("returned %x, want a 32-byte word ending in 05", r.Ret)
	}
}

// An infinite loop runs out of gas rather than forever, and burns all of it.
func TestInfiniteLoopRunsOutOfGas(t *testing.T) {
	code := b(
		byte(JUMPDEST),    // 0
		byte(PUSH1), 0x00, // 1-2: push jump target 0
		byte(JUMP), // 3: back to 0, forever
	)
	r := Run(code, nil, 1000, Context{})
	if !errors.Is(r.Err, ErrOutOfGas) {
		t.Fatalf("got %v, want ErrOutOfGas", r.Err)
	}
	if r.GasLeft != 0 {
		t.Fatalf("a hard fault must consume all gas, %d left", r.GasLeft)
	}
}

// REVERT returns its data and signals the error, and unlike a hard fault
// refunds the unspent gas.
func TestRevertReturnsDataAndRefundsGas(t *testing.T) {
	code := b(
		byte(PUSH1), 0xff,
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(REVERT),
	)
	r := Run(code, nil, 100_000, Context{})
	if !errors.Is(r.Err, ErrReverted) {
		t.Fatalf("got %v, want ErrReverted", r.Err)
	}
	if len(r.Ret) != 32 || r.Ret[31] != 0xff {
		t.Fatalf("revert data %x, want a word ending in ff", r.Ret)
	}
	if r.GasLeft == 0 {
		t.Fatal("REVERT should refund the remaining gas, not burn it")
	}
}

func TestStackUnderflowFaults(t *testing.T) {
	r := Run(b(byte(ADD)), nil, 1000, Context{}) // ADD with an empty stack
	if !errors.Is(r.Err, ErrStackUnderflow) {
		t.Fatalf("got %v, want ErrStackUnderflow", r.Err)
	}
}

// Division by zero is defined as zero in the EVM, not a fault.
func TestDivByZeroIsZero(t *testing.T) {
	code := b(
		byte(PUSH1), 0x00, // divisor (second from top)
		byte(PUSH1), 0x06, // dividend (top)
		byte(DIV), // 6 / 0 -> 0
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	)
	r := Run(code, nil, 100_000, Context{})
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	for _, x := range r.Ret {
		if x != 0 {
			t.Fatalf("6/0 returned %x, want zero", r.Ret)
		}
	}
}

// A JUMP into the middle of PUSH data, or anywhere that is not a JUMPDEST, is
// refused: the guard against jumping into a constant and running it as code.
func TestJumpToNonDestFaults(t *testing.T) {
	code := b(
		byte(PUSH1), 0x02, // target 2: the 0x02 data byte of this PUSH
		byte(JUMP),
	)
	r := Run(code, nil, 1000, Context{})
	if !errors.Is(r.Err, ErrInvalidJump) {
		t.Fatalf("got %v, want ErrInvalidJump", r.Err)
	}
}

// JUMPI branches only when the condition is nonzero.
func TestJumpiTakesBranchWhenNonzero(t *testing.T) {
	// if 1: jump to the JUMPDEST that returns 0xaa; else fall through to STOP.
	code := b(
		byte(PUSH1), 0x01, // condition = 1 (top after next push? see order)
		byte(PUSH1), 0x08, // dest = 8
		byte(JUMPI),    // 4: pop dest=8, cond=1 -> jump
		byte(STOP),     // 5 (skipped)
		byte(INVALID),  // 6 (skipped)
		byte(INVALID),  // 7 (skipped)
		byte(JUMPDEST), // 8
		byte(PUSH1), 0xaa,
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	)
	r := Run(code, nil, 100_000, Context{})
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if len(r.Ret) != 32 || r.Ret[31] != 0xaa {
		t.Fatalf("returned %x, want a word ending in aa", r.Ret)
	}
}
