package vm

import (
	"bytes"
	"math/big"
	"testing"
)

// TestExtCodeCopyLiftsAnotherAccountsCode checks EXTCODECOPY reads a different
// account's runtime bytes into memory (unlike CODECOPY, which reads the running
// code). A wrong stack order or a copy from self rather than the target would
// only surface on a real solc contract.
func TestExtCodeCopyLiftsAnotherAccountsCode(t *testing.T) {
	state := newMockState()
	self := addr(0xAA)
	other := addr(0xCC)
	// A distinctive runtime so a copy from the wrong source cannot match.
	runtime := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03}
	state.SetCode(other, runtime)

	ctx := Context{Address: self, State: state}

	// EXTCODECOPY pops addr, memOff, codeOff, length (addr on top), so push them
	// in reverse. Copy the whole code to memory offset 0, then RETURN 32 bytes.
	code := []byte{
		byte(PUSH1), byte(len(runtime)), // length
		byte(PUSH1), 0x00, // codeOff
		byte(PUSH1), 0x00, // memOff
		byte(PUSH1), 0xCC, // addr
		byte(EXTCODECOPY),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	}
	r := Run(code, nil, 1_000_000, ctx)
	if r.Err != nil {
		t.Fatalf("EXTCODECOPY execution failed: %v", r.Err)
	}
	if len(r.Ret) != 32 {
		t.Fatalf("expected 32-byte return, got %d", len(r.Ret))
	}
	if !bytes.Equal(r.Ret[:len(runtime)], runtime) {
		t.Errorf("copied code = %x, want prefix %x", r.Ret[:len(runtime)], runtime)
	}
	// Bytes past the real code must be zero-padded: EXTCODECOPY of a range
	// longer than the code fills the tail with zeros.
	for i := len(runtime); i < 32; i++ {
		if r.Ret[i] != 0 {
			t.Errorf("byte %d past end = %#x, want 0 (must zero-pad)", i, r.Ret[i])
		}
	}
}

// TestExtCodeCopyFromCodelessAccountIsZeros: copying from a code-less account
// (an EOA, or an untouched address) yields zeros and does not fault, mirroring
// EXTCODESIZE's view of such an account as length 0. Faulting here would revert
// transactions mainnet accepts.
func TestExtCodeCopyFromCodelessAccountIsZeros(t *testing.T) {
	state := newMockState()
	ctx := Context{Address: addr(0xAA), State: state}

	code := []byte{
		byte(PUSH1), 0x20, // length
		byte(PUSH1), 0x00, // codeOff
		byte(PUSH1), 0x00, // memOff
		byte(PUSH1), 0xBB, // addr (never given code)
		byte(EXTCODECOPY),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	}
	r := Run(code, nil, 1_000_000, ctx)
	if r.Err != nil {
		t.Fatalf("EXTCODECOPY of codeless account faulted: %v", r.Err)
	}
	if got := new(big.Int).SetBytes(r.Ret); got.Sign() != 0 {
		t.Errorf("EXTCODECOPY(EOA) = %x, want all zeros", r.Ret)
	}
}

// TestExtCodeCopyChargesPerWordGas checks the per-word copy cost is charged on
// top of the 700 base and memory expansion, not just the flat base (else a
// contract could copy arbitrarily large code for a flat fee). Two copies of
// different lengths must cost different amounts of gas.
func TestExtCodeCopyChargesPerWordGas(t *testing.T) {
	state := newMockState()
	big1 := make([]byte, 64) // 2 words
	for i := range big1 {
		big1[i] = 0x11
	}
	state.SetCode(addr(0xCC), big1)
	ctx := Context{Address: addr(0xAA), State: state}

	copyN := func(n byte) uint64 {
		code := []byte{
			// Pre-expand memory to 160 bytes (MSTORE a word at offset 128) so the
			// EXTCODECOPY below, writing within [0,64), triggers no further
			// expansion. With expansion held equal across both runs, the only
			// length-dependent cost left is the per-word copy charge; if it is
			// dropped, the two runs cost the same and the assertion fires.
			byte(PUSH1), 0x00, byte(PUSH1), 0x80, byte(MSTORE),
			byte(PUSH1), n, // length
			byte(PUSH1), 0x00, // codeOff
			byte(PUSH1), 0x00, // memOff
			byte(PUSH1), 0xCC, // addr
			byte(EXTCODECOPY), byte(STOP),
		}
		const budget = 1_000_000
		r := Run(code, nil, budget, ctx)
		if r.Err != nil {
			t.Fatalf("EXTCODECOPY(len=%d) failed: %v", n, r.Err)
		}
		return budget - r.GasLeft // gas actually consumed
	}
	gasSmall := copyN(1)  // 1 word copied
	gasLarge := copyN(64) // 2 words copied: 3 gas dearer, nothing else differs
	if gasLarge <= gasSmall {
		t.Errorf("gas for 64-byte copy (%d) must exceed 1-byte copy (%d): per-word cost not charged", gasLarge, gasSmall)
	}
}
