package vm

import (
	"math/big"
	"testing"

	"lxs/common"
)

// runWord runs code that RETURNs a 32-byte word against the given context.
func runWord(t *testing.T, code []byte, ctx Context) *big.Int {
	t.Helper()
	r := Run(code, nil, 1_000_000, ctx)
	if r.Err != nil {
		t.Fatalf("execution failed: %v", r.Err)
	}
	if len(r.Ret) != 32 {
		t.Fatalf("expected a 32-byte return, got %d", len(r.Ret))
	}
	return new(big.Int).SetBytes(r.Ret)
}

// retWord appends the "store word at 0 and RETURN it" epilogue.
func retWord(prefix ...byte) []byte {
	return append(prefix, byte(PUSH1), 0x00, byte(MSTORE), byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))
}

// TestSignedComparisons checks SLT/SGT interpret operands as two's complement,
// unlike LT/GT which see -1 as a large unsigned number.
func TestSignedComparisons(t *testing.T) {
	cases := []struct {
		name       string
		op         OpCode
		top, below *big.Int
		want       int64
	}{
		{"SLT -1 < 0", SLT, word(-1), word(0), 1},
		{"SLT 0 < -1", SLT, word(0), word(-1), 0},
		{"SGT 0 > -1", SGT, word(0), word(-1), 1},
		{"SGT -1 > 0", SGT, word(-1), word(0), 0},
		// vs unsigned LT: -1 (0xff..ff) is the largest unsigned value, so
		// LT(-1,0)=0 but SLT(-1,0)=1.
		{"LT -1 < 0 (unsigned, false)", LT, word(-1), word(0), 0},
	}
	for _, c := range cases {
		if got := evalBin(t, c.op, c.top, c.below); got.Int64() != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got.Int64(), c.want)
		}
	}
}

// TestAccountInspection covers BALANCE / SELFBALANCE / EXTCODESIZE /
// EXTCODEHASH: how a contract reads other accounts.
func TestAccountInspection(t *testing.T) {
	state := newMockState()
	self := addr(0xAA)
	other := addr(0xBB)
	contract := addr(0xCC)
	state.AddBalance(self, big.NewInt(999))
	state.AddBalance(other, big.NewInt(12345))
	runtime := []byte{0x60, 0x00, 0x60, 0x00, 0x55, 0x00} // non-empty code
	state.SetCode(contract, runtime)

	ctx := Context{Address: self, State: state}

	// BALANCE of another account.
	if got := runWord(t, retWord(byte(PUSH1), 0xBB, byte(BALANCE)), ctx); got.Int64() != 12345 {
		t.Errorf("BALANCE(0xBB) = %s, want 12345", got)
	}
	// SELFBALANCE reads the running contract's own balance.
	if got := runWord(t, retWord(byte(SELFBALANCE)), ctx); got.Int64() != 999 {
		t.Errorf("SELFBALANCE = %s, want 999", got)
	}
	// EXTCODESIZE of a contract.
	if got := runWord(t, retWord(byte(PUSH1), 0xCC, byte(EXTCODESIZE)), ctx); got.Int64() != int64(len(runtime)) {
		t.Errorf("EXTCODESIZE(0xCC) = %s, want %d", got, len(runtime))
	}
	// EXTCODESIZE of a code-less account is 0 (isContract == false).
	if got := runWord(t, retWord(byte(PUSH1), 0xBB, byte(EXTCODESIZE)), ctx); got.Sign() != 0 {
		t.Errorf("EXTCODESIZE(EOA) = %s, want 0", got)
	}
	// EXTCODEHASH of a contract = keccak256(code).
	wantHash := new(big.Int).SetBytes(common.Keccak256(runtime).Bytes())
	if got := runWord(t, retWord(byte(PUSH1), 0xCC, byte(EXTCODEHASH)), ctx); got.Cmp(wantHash) != 0 {
		t.Errorf("EXTCODEHASH(0xCC) = %x, want %x", got, wantHash)
	}
	// EXTCODEHASH of an EXISTING code-less account (a funded EOA) = keccak256("") per EIP-1052.
	wantEmpty := new(big.Int).SetBytes(common.Keccak256(nil).Bytes())
	if got := runWord(t, retWord(byte(PUSH1), 0xBB, byte(EXTCODEHASH)), ctx); got.Cmp(wantEmpty) != 0 {
		t.Errorf("EXTCODEHASH(EOA) = %x, want empty-code hash %x", got, wantEmpty)
	}
	// EXTCODEHASH of a truly non-existent account = 0 (the EIP-1052 distinction).
	if got := runWord(t, retWord(byte(PUSH1), 0xDE, byte(EXTCODEHASH)), ctx); got.Sign() != 0 {
		t.Errorf("EXTCODEHASH(non-existent) = %s, want 0", got)
	}
}
