package vm

import "testing"

// TestGasScheduleIsIstanbul pins every implemented opcode's static gas to its
// Istanbul value. A mispriced opcode is both a consensus split (nodes disagree
// on gasUsed) and a DoS vector (an underpriced op is cheap to spam).
//
// The want-values are literals, not derived from the same quick/fastest/...
// constants the implementation uses, so a change to those constants cannot drag
// the expectations along with it.
func TestGasScheduleIsIstanbul(t *testing.T) {
	want := map[OpCode]uint64{
		// G_zero: free static (do nothing, or pay only dynamically).
		STOP: 0, RETURN: 0, REVERT: 0, INVALID: 0,
		// Charged entirely at execution time, so their table entry is 0.
		SSTORE: 0, CALL: 0, DELEGATECALL: 0, STATICCALL: 0, CREATE: 0, CREATE2: 0,

		// G_base = 2.
		ADDRESS: 2, CALLER: 2, CALLVALUE: 2, CALLDATASIZE: 2, GASLIMIT: 2,
		POP: 2, PC: 2, MSIZE: 2, GAS: 2,
		// G_base = 2 (block/tx context).
		ORIGIN: 2, GASPRICE: 2, COINBASE: 2, TIMESTAMP: 2,
		NUMBER: 2, DIFFICULTY: 2, CHAINID: 2, BASEFEE: 2,
		BLOCKHASH: 20, // G_blockhash

		// G_base = 2 (cont.): RETURNDATASIZE/CODESIZE just read a length.
		RETURNDATASIZE: 2, CODESIZE: 2, PUSH0: 2,
		// LOG0..LOG4 are 0 static: whole cost is dynamic (base + topics + data),
		// charged in opLog.
		LOG0: 0, LOG0 + 4: 0,

		// G_verylow = 3.
		ADD: 3, SUB: 3, NOT: 3, LT: 3, GT: 3, EQ: 3, ISZERO: 3,
		AND: 3, OR: 3, XOR: 3, BYTE: 3, SHL: 3, SHR: 3, SAR: 3, SLT: 3, SGT: 3,
		CALLDATALOAD: 3, CALLDATACOPY: 3, RETURNDATACOPY: 3, CODECOPY: 3,
		MLOAD: 3, MSTORE: 3, MSTORE8: 3, MCOPY: 3,
		PUSH1: 3, PUSH32: 3, DUP1: 3, DUP16: 3, SWAP1: 3, SWAP16: 3,

		// G_low = 5.
		MUL: 5, DIV: 5, MOD: 5, SDIV: 5, SMOD: 5, SIGNEXTEND: 5, SELFBALANCE: 5,
		BALANCE: 700, EXTCODESIZE: 700, EXTCODEHASH: 700, EXTCODECOPY: 700,

		// G_mid = 8.
		ADDMOD: 8, MULMOD: 8, JUMP: 8,

		// G_high = 10 (EXP is 10 base + 50 per exponent byte, charged dynamically).
		JUMPI: 10, EXP: 10,

		JUMPDEST: 1,
		SLOAD:    800, // Istanbul EIP-1884
		SHA3:     30,  // + 6 per word, dynamic
	}
	for op, w := range want {
		if got := gasCost[op]; got != w {
			t.Errorf("gasCost[%#x] = %d, want %d", byte(op), got, w)
		}
	}
}

// TestMemoryExpansionCost pins the quadratic memory price against hand-computed
// values: C(w) = 3*w + w^2/512. The quadratic term is what bounds a contract's
// memory request, so its exact shape is load-bearing.
func TestMemoryExpansionCost(t *testing.T) {
	cases := []struct {
		words uint64
		want  uint64
	}{
		{0, 0},
		{1, 3},       // 3 + 0
		{2, 6},       // 6 + 4/512(=0)
		{32, 98},     // 96 + 1024/512(=2)
		{100, 319},   // 300 + 10000/512(=19)
		{1024, 5120}, // 3072 + 1048576/512(=2048)
	}
	for _, c := range cases {
		if got := memoryCost(c.words); got != c.want {
			t.Errorf("memoryCost(%d) = %d, want %d", c.words, got, c.want)
		}
	}

	// expansionGas is the delta to reach a new size from empty memory.
	m := NewMemory()
	if g, ok := m.expansionGas(0, 32); !ok || g != 3 {
		t.Errorf("grow to 32 bytes (1 word): got %d ok=%v, want 3", g, ok)
	}
	if g, ok := m.expansionGas(0, 33); !ok || g != 6 {
		t.Errorf("grow to 33 bytes (2 words): got %d ok=%v, want 6", g, ok)
	}
	if g, ok := m.expansionGas(0, 0); !ok || g != 0 {
		t.Errorf("zero-size access must be free: got %d ok=%v", g, ok)
	}
}

// TestExpChargesPerExponentByte checks the EXP dynamic cost is 50 gas per byte
// of the exponent (EIP-160), so a small exponent literal cannot buy an
// expensive modexp for a flat fee.
func TestExpChargesPerExponentByte(t *testing.T) {
	// exponent 256 = 0x0100 (2 bytes), base 2. Cost:
	//   PUSH2 (3) + PUSH1 (3) + EXP static (10) + 50*2 (100) = 116.
	code := b(
		byte(PUSH1+1), 0x01, 0x00, // PUSH2 exponent 0x0100 (below)
		byte(PUSH1), 0x02, // base (top)
		byte(EXP),
		byte(STOP),
	)
	const start = 100_000
	r := Run(code, nil, start, Context{})
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if used := start - r.GasLeft; used != 116 {
		t.Fatalf("EXP with a 2-byte exponent used %d gas, want 116", used)
	}
}

// TestSHA3ChargesPerWord checks KECCAK256 pays a per-word surcharge on the
// hashed length on top of its flat base and the memory it touches, so a contract
// cannot hash megabytes for 30 gas.
func TestSHA3ChargesPerWord(t *testing.T) {
	// Hash 32 bytes at offset 0. Cost:
	//   PUSH1 size (3) + PUSH1 off (3) + SHA3 base (30)
	//   + memory expansion for 1 word (3) + 6*1 word (6) = 45.
	code := b(
		byte(PUSH1), 0x20, // size 32 (below)
		byte(PUSH1), 0x00, // offset 0 (top)
		byte(SHA3),
		byte(STOP),
	)
	const start = 100_000
	r := Run(code, nil, start, Context{})
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if used := start - r.GasLeft; used != 45 {
		t.Fatalf("SHA3 over 32 bytes used %d gas, want 45", used)
	}
}
