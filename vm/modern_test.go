package vm

import (
	"math/big"
	"testing"
)

// TestPush0 checks the Shanghai PUSH0: it pushes a zero and is a single byte,
// so it must not swallow the following opcode (a common decoder bug).
func TestPush0(t *testing.T) {
	// PUSH0 ; PUSH1 5 ; ADD  ->  should be 5, proving PUSH0 pushed 0 and pc
	// advanced by exactly one so the PUSH1 5 still ran.
	code := b(
		byte(PUSH0), byte(PUSH1), 0x05, byte(ADD),
		byte(PUSH1), 0x00, byte(MSTORE), byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	)
	r := Run(code, nil, 100_000, Context{})
	if r.Err != nil || len(r.Ret) != 32 || r.Ret[31] != 5 {
		t.Fatalf("PUSH0 then +5 = %x (err %v), want a word ending in 05", r.Ret, r.Err)
	}
}

// TestMcopy checks the Cancun MCOPY: memory copied from one region to another.
func TestMcopy(t *testing.T) {
	// mem[0] = 0xdead ; MCOPY(dest=0x20, src=0x00, len=0x20) ; return mem[0x20].
	code := append(push32(big.NewInt(0xdead)), byte(PUSH1), 0x00, byte(MSTORE))
	code = append(code,
		byte(PUSH1), 0x20, // length
		byte(PUSH1), 0x00, // src
		byte(PUSH1), 0x20, // dest
		byte(MCOPY),
		byte(PUSH1), 0x20, byte(PUSH1), 0x20, byte(RETURN)) // return mem[0x20:0x40]

	r := Run(code, nil, 100_000, Context{})
	if r.Err != nil {
		t.Fatalf("MCOPY failed: %v", r.Err)
	}
	if got := new(big.Int).SetBytes(r.Ret); got.Int64() != 0xdead {
		t.Fatalf("MCOPY result = %#x, want 0xdead", got)
	}
}

// TestMcopyOverlap checks an overlapping copy is still correct: MCOPY behaves as
// if the source were read in full before the destination is written.
func TestMcopyOverlap(t *testing.T) {
	// mem[0]=0x11..(word), then MCOPY(dest=0x10, src=0x00, len=0x20): the
	// destination overlaps the source. Return mem[0x10:0x30] — it must equal the
	// original word regardless of overlap.
	word := new(big.Int)
	word.SetString("1122334455667788990011223344556677889900112233445566778899001122", 16)
	code := append(push32(word), byte(PUSH1), 0x00, byte(MSTORE))
	code = append(code,
		byte(PUSH1), 0x20, // length
		byte(PUSH1), 0x00, // src
		byte(PUSH1), 0x10, // dest (overlaps)
		byte(MCOPY),
		byte(PUSH1), 0x20, byte(PUSH1), 0x10, byte(RETURN))

	r := Run(code, nil, 100_000, Context{})
	if r.Err != nil {
		t.Fatalf("overlapping MCOPY failed: %v", r.Err)
	}
	if new(big.Int).SetBytes(r.Ret).Cmp(word) != 0 {
		t.Fatalf("overlapping MCOPY corrupted the data: got %x", r.Ret)
	}
}
