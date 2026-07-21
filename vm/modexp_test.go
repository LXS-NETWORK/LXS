package vm

import (
	"bytes"
	"encoding/hex"
	"testing"

	"lxs/common"
)

// Vectors lifted from go-ethereum's core/vm/testdata/precompiles/modexp.json. The
// gas column pins the Istanbul (EIP-198) pricing: nagydani-1-square is 204 here,
// where the cheaper EIP-2565/Berlin schedule would floor it at 200 — so a wrong
// fork's gas table fails this test instead of silently diverging from peers.
var modexpVectors = []struct {
	name   string
	input  string
	expect string
	gas    uint64
}{
	{
		name:   "eip_example1",
		input:  "00000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000002003fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2efffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f",
		expect: "0000000000000000000000000000000000000000000000000000000000000001",
		gas:    13056,
	},
	{
		name:   "nagydani-1-square",
		input:  "000000000000000000000000000000000000000000000000000000000000004000000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000040e09ad9675465c53a109fac66a445c91b292d2bb2c5268addb30cd82f80fcb0033ff97c80a5fc6f39193ae969c6ede6710a6b7ac27078a06d90ef1c72e5c85fb502fc9e1f6beb81516545975218075ec2af118cd8798df6e08a147c60fd6095ac2bb02c2908cf4dd7c81f11c289e4bce98f3553768f392a80ce22bf5c4f4a248c6b",
		expect: "60008f1614cc01dcfb6bfb09c625cf90b47d4468db81b5f8b7a39d42f332eab9b2da8f2d95311648a8f243f4bb13cfb3d8f7f2a3c014122ebb3ed41b02783adc",
		gas:    204,
	},
}

func TestModexpVectors(t *testing.T) {
	for _, v := range modexpVectors {
		input, _ := hex.DecodeString(v.input)
		want, _ := hex.DecodeString(v.expect)

		if got, _ := modexpRun(input); !bytes.Equal(got, want) {
			t.Errorf("%s: modexp = %x, want %x", v.name, got, want)
		}
		if got := modexpGas(input); got != v.gas {
			t.Errorf("%s: gas = %d, want %d (EIP-198/Istanbul)", v.name, got, v.gas)
		}
	}
}

// The whole point of this audit item: before it, a CALL to 0x05 hit NO precompile,
// so the address behaved as an empty account — the call "succeeded" and returned
// nothing, and any contract that trusted the modexp result (an RSA/zk verifier)
// would silently accept a forgery. 0x05 must now resolve to the modexp precompile.
func TestModexpAddressIsWired(t *testing.T) {
	var addr common.Address
	addr[len(addr)-1] = 5
	if precompileFor(addr) == nil {
		t.Fatal("0x05 resolves to no precompile — a CALL returns empty+success, the landmine")
	}
	// A neighbouring non-precompile address must still be a plain account.
	var notPre common.Address
	notPre[len(notPre)-1] = 5
	notPre[0] = 1 // high byte set -> not a precompile address
	if precompileFor(notPre) != nil {
		t.Fatal("an address with a non-zero high byte must not be treated as a precompile")
	}
}

// modLen == 0 yields an empty result, and a small hand-checkable case (3**2 % 5 = 4)
// confirms the arithmetic and modLen-width left-padded output.
func TestModexpEdgeCases(t *testing.T) {
	// baseLen=1 expLen=1 modLen=1, base=3 exp=2 mod=5 -> 4.
	in := make([]byte, 96)
	in[31], in[63], in[95] = 1, 1, 1
	in = append(in, 0x03, 0x02, 0x05)
	if got, _ := modexpRun(in); len(got) != 1 || got[0] != 4 {
		t.Fatalf("3**2 %% 5 = %x, want 04", got)
	}

	// modLen = 0 -> empty output.
	zero := make([]byte, 96)
	zero[31] = 1 // baseLen 1, modLen 0
	zero = append(zero, 0x07)
	if got, _ := modexpRun(zero); len(got) != 0 {
		t.Fatalf("modLen 0 output = %x, want empty", got)
	}
}
