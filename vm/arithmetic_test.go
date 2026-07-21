package vm

import (
	"bytes"
	"math/big"
	"testing"

	"lxs/common"
)

// word returns the two's-complement 256-bit encoding of a (possibly negative)
// small integer, as a signed value sits on the EVM stack.
func word(n int64) *big.Int {
	x := big.NewInt(n)
	if n < 0 {
		x.Add(x, tt256) // -k becomes 2^256 - k
	}
	return x
}

// push32 emits PUSH32 <32 big-endian bytes> for x.
func push32(x *big.Int) []byte {
	var buf [32]byte
	x.FillBytes(buf[:]) // big-endian, left-zero-padded
	return append([]byte{byte(PUSH32)}, buf[:]...)
}

// runReturn runs code that RETURNs a 32-byte word and gives back that word.
func runReturn(t *testing.T, code []byte) *big.Int {
	t.Helper()
	r := Run(code, nil, 1_000_000, Context{})
	if r.Err != nil {
		t.Fatalf("execution failed: %v", r.Err)
	}
	if len(r.Ret) != 32 {
		t.Fatalf("expected a 32-byte return, got %d bytes", len(r.Ret))
	}
	return new(big.Int).SetBytes(r.Ret)
}

// evalBin runs `op` with `top` at the stack top and `below` beneath it, then
// returns the result word. For DIV/SDIV/MOD/SMOD `top` is the dividend; for the
// shifts `top` is the shift amount.
func evalBin(t *testing.T, op OpCode, top, below *big.Int) *big.Int {
	t.Helper()
	code := append(push32(below), push32(top)...)
	code = append(code, byte(op),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))
	return runReturn(t, code)
}

// TestSignedArithmetic covers the two's-complement opcodes. The edge case
// INT_MIN / -1 is included: it overflows a signed 256-bit word and must wrap
// rather than fault.
func TestSignedArithmetic(t *testing.T) {
	intMin := new(big.Int).Set(tt255) // -2^255 as an unsigned word (0x8000..00)
	negOne := new(big.Int).Set(tt256m1)

	cases := []struct {
		name       string
		op         OpCode
		top, below *big.Int
		want       *big.Int
	}{
		{"SDIV -4/2", SDIV, word(-4), word(2), word(-2)},
		{"SDIV 7/-2", SDIV, word(7), word(-2), word(-3)},
		// -7/2 distinguishes truncation-toward-zero (EVM: -3) from Euclidean/floor
		// division (-4); the reason the impl uses Quo, not Div.
		{"SDIV -7/2 trunc toward 0", SDIV, word(-7), word(2), word(-3)},
		{"SDIV by zero", SDIV, word(5), word(0), word(0)},
		{"SDIV INT_MIN/-1 wraps", SDIV, intMin, negOne, intMin},
		{"SMOD -8%3 sign of dividend", SMOD, word(-8), word(3), word(-2)},
		{"SMOD 8%-3", SMOD, word(8), word(-3), word(2)},
		{"SMOD by zero", SMOD, word(7), word(0), word(0)},
		{"SAR -4>>1", SAR, word(1), word(-4), word(-2)},
		{"SAR -1>>256 stays -1", SAR, word(256), word(-1), negOne},
		{"SAR 8>>256 -> 0", SAR, word(256), word(8), word(0)},
		{"SAR 1>>1", SAR, word(1), word(1), word(0)},
	}
	for _, c := range cases {
		if got := evalBin(t, c.op, c.top, c.below); got.Cmp(c.want) != 0 {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// TestModWithoutWrapping checks ADDMOD/MULMOD keep full precision: the
// intermediate is reduced by n without first wrapping at 2^256. The expected
// values are chosen so a wrap-first implementation would disagree.
func TestModWithoutWrapping(t *testing.T) {
	max := new(big.Int).Set(tt256m1) // 2^256 - 1
	two128 := new(big.Int).Lsh(big.NewInt(1), 128)

	// addmod(MAX, 2, 5): full precision (2^256+1) mod 5 = 2. Wrapped: 1 mod 5 = 1.
	if got := evalTri(t, ADDMOD, max, big.NewInt(2), big.NewInt(5)); got.Cmp(big.NewInt(2)) != 0 {
		t.Errorf("ADDMOD full precision: got %s, want 2", got)
	}
	// mulmod(2^128, 2^128, 7): full precision 2^256 mod 7 = 2. Wrapped: 0 mod 7 = 0.
	if got := evalTri(t, MULMOD, two128, two128, big.NewInt(7)); got.Cmp(big.NewInt(2)) != 0 {
		t.Errorf("MULMOD full precision: got %s, want 2", got)
	}
	// mod by zero is zero.
	if got := evalTri(t, ADDMOD, big.NewInt(3), big.NewInt(4), big.NewInt(0)); got.Sign() != 0 {
		t.Errorf("ADDMOD by zero: got %s, want 0", got)
	}
}

// evalTri runs a three-operand op: a on top, then b, then n.
func evalTri(t *testing.T, op OpCode, a, b, n *big.Int) *big.Int {
	t.Helper()
	code := append(push32(n), push32(b)...)
	code = append(code, push32(a)...)
	code = append(code, byte(op),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))
	return runReturn(t, code)
}

// TestSignextendAndByte checks the two "reach into a word" opcodes.
func TestSignextendAndByte(t *testing.T) {
	// SIGNEXTEND(0, 0x80): byte 0 is 0x80, sign bit set -> every higher bit
	// becomes 1, giving ...FFFF80 (which is -128 read as signed).
	wantExt := new(big.Int).Sub(tt256, big.NewInt(0x80)) // ...FFFF80
	if got := evalBin(t, SIGNEXTEND, big.NewInt(0), big.NewInt(0x80)); got.Cmp(wantExt) != 0 {
		t.Errorf("SIGNEXTEND(0,0x80): got %x, want %x", got, wantExt)
	}
	// SIGNEXTEND(0, 0x7f): sign bit clear -> unchanged.
	if got := evalBin(t, SIGNEXTEND, big.NewInt(0), big.NewInt(0x7f)); got.Cmp(big.NewInt(0x7f)) != 0 {
		t.Errorf("SIGNEXTEND(0,0x7f): got %s, want 127", got)
	}
	// SIGNEXTEND(31, x): sign byte already the top byte -> unchanged.
	big5 := big.NewInt(5)
	if got := evalBin(t, SIGNEXTEND, big.NewInt(31), big5); got.Cmp(big5) != 0 {
		t.Errorf("SIGNEXTEND(31,5): got %s, want 5", got)
	}
	// BYTE: index counts from the most-significant byte.
	val := big.NewInt(0x1122)
	if got := evalBin(t, BYTE, big.NewInt(31), val); got.Cmp(big.NewInt(0x22)) != 0 {
		t.Errorf("BYTE(31): got %x, want 22", got)
	}
	if got := evalBin(t, BYTE, big.NewInt(30), val); got.Cmp(big.NewInt(0x11)) != 0 {
		t.Errorf("BYTE(30): got %x, want 11", got)
	}
	if got := evalBin(t, BYTE, big.NewInt(32), val); got.Sign() != 0 {
		t.Errorf("BYTE(32) out of range: got %s, want 0", got)
	}
}

// TestSHA3 verifies the KECCAK256 opcode against a known-answer vector (the
// empty-input digest) and against a hashed 32-byte memory window (the shape
// Solidity uses to place a mapping entry).
func TestSHA3(t *testing.T) {
	// keccak256(""): the canonical empty-input digest.
	emptyHash := "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
	code := b(
		byte(PUSH1), 0x00, // size 0
		byte(PUSH1), 0x00, // offset 0
		byte(SHA3),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	)
	got := runReturn(t, code)
	if got.Text(16) != emptyHash {
		t.Fatalf("SHA3(\"\") = %s, want %s", got.Text(16), emptyHash)
	}

	// Hash a full 32-byte word held in memory; cross-check against the shared
	// keccak library.
	input := new(big.Int).SetInt64(0x42)
	var buf [32]byte
	input.FillBytes(buf[:])
	code2 := append(push32(input),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(SHA3),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))
	want := common.Keccak256(buf[:])
	if got := runReturn(t, code2); !bytes.Equal(got.Bytes(), bytesTrimLeft(want.Bytes())) {
		t.Fatalf("SHA3(word) = %x, want %x", got.Bytes(), want.Bytes())
	}
}

// bytesTrimLeft drops leading zero bytes so a big.Int round-trip (which loses
// them) compares equal to a fixed-width hash.
func bytesTrimLeft(b []byte) []byte {
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return b[i:]
}
