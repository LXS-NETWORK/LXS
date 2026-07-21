package vm

import (
	"math/big"
	"testing"
)

// TestBlockAndTxContext checks the block/tx environment opcodes read from the
// Context. These back Solidity's block.number / block.timestamp /
// block.chainid / tx.origin / tx.gasprice.
func TestBlockAndTxContext(t *testing.T) {
	ctx := Context{
		Address:     addr(0xAA),
		State:       newMockState(),
		Origin:      addr(0x11),
		GasPrice:    big.NewInt(7),
		Coinbase:    addr(0x22),
		BlockNumber: 42,
		Time:        1_700_000_000,
		Difficulty:  big.NewInt(99),
		ChainID:     1337,
		// BaseFee left nil: BASEFEE must still push 0, not panic.
	}

	cases := []struct {
		name string
		op   OpCode
		want *big.Int
	}{
		{"NUMBER", NUMBER, big.NewInt(42)},
		{"TIMESTAMP", TIMESTAMP, big.NewInt(1_700_000_000)},
		{"CHAINID", CHAINID, big.NewInt(1337)},
		{"DIFFICULTY", DIFFICULTY, big.NewInt(99)},
		{"GASPRICE", GASPRICE, big.NewInt(7)},
		{"BASEFEE", BASEFEE, big.NewInt(0)},
		{"ORIGIN", ORIGIN, new(big.Int).SetBytes(addr(0x11).Bytes())},
		{"COINBASE", COINBASE, new(big.Int).SetBytes(addr(0x22).Bytes())},
	}
	for _, c := range cases {
		got := runWord(t, retWord(byte(c.op)), ctx)
		if got.Cmp(c.want) != 0 {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}

	// BLOCKHASH pops a block number and returns 0 (documented simplification).
	if got := runWord(t, retWord(byte(PUSH1), 0x05, byte(BLOCKHASH)), ctx); got.Sign() != 0 {
		t.Errorf("BLOCKHASH = %s, want 0", got)
	}
}

// TestContextInheritedByCall checks the block/tx environment carries unchanged
// into a sub-call: a callee reading NUMBER/ORIGIN sees the same block and
// original sender as the top frame, even though msg.sender changed.
func TestContextInheritedByCall(t *testing.T) {
	state := newMockState()
	caller := addr(0xAA)
	callee := addr(0xBB)
	// Callee returns NUMBER.
	state.SetCode(callee, b(
		byte(NUMBER), byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	))

	// Caller CALLs callee and returns the callee's output via RETURNDATACOPY.
	code := append(callTo(0x00, 0xBB),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(RETURNDATACOPY),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))

	ctx := Context{Address: caller, State: state, BlockNumber: 777, Origin: addr(0x11)}
	if got := runWord(t, code, ctx); got.Int64() != 777 {
		t.Fatalf("callee NUMBER = %s, want 777 (block context not inherited by CALL)", got)
	}
}
