package vm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/crypto"
)

// TestRIPEMD160KnownAnswers pins the from-scratch hash against published
// RIPEMD-160 test vectors. A wrong table entry produces plausible garbage
// without crashing, so only a known-answer vector catches it.
func TestRIPEMD160KnownAnswers(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "9c1185a5c5e9fc54612808977ee8f548b2258d31"},
		{"abc", "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc"},
		{"message digest", "5d0689ef49d2fae572b881b123a85ffa21595f36"},
		{"abcdefghijklmnopqrstuvwxyz", "f71c27109c692c1b56bbdceb5b9d2865b3708dbc"},
	}
	for _, c := range cases {
		got := ripemd160([]byte(c.in))
		if hex.EncodeToString(got[:]) != c.want {
			t.Errorf("ripemd160(%q) = %x, want %s", c.in, got, c.want)
		}
	}
}

// TestHashPrecompiles checks 0x02/0x03/0x04 through runPrecompile: output bytes
// and the EVM word layout (RIPEMD-160 left-padded into 32 bytes).
func TestHashPrecompiles(t *testing.T) {
	input := []byte("the quick brown fox")

	// 0x04 identity: bytes back unchanged.
	if got := runPrecompile(precompileFor(addr(4)), input, 1_000_000); !bytes.Equal(got.Ret, input) {
		t.Errorf("identity returned %x, want %x", got.Ret, input)
	}

	// 0x02 sha256: 32-byte digest, cross-checked against the stdlib.
	wantSha := sha256.Sum256(input)
	if got := runPrecompile(precompileFor(addr(2)), input, 1_000_000); !bytes.Equal(got.Ret, wantSha[:]) {
		t.Errorf("sha256 returned %x, want %x", got.Ret, wantSha)
	}

	// 0x03 ripemd160: 20-byte digest LEFT-padded into a 32-byte word.
	got := runPrecompile(precompileFor(addr(3)), input, 1_000_000)
	if len(got.Ret) != 32 {
		t.Fatalf("ripemd160 precompile returned %d bytes, want 32", len(got.Ret))
	}
	for i := 0; i < 12; i++ {
		if got.Ret[i] != 0 {
			t.Fatalf("ripemd160 output not left-padded: byte %d = %x", i, got.Ret[i])
		}
	}
	want := ripemd160(input)
	if !bytes.Equal(got.Ret[12:], want[:]) {
		t.Errorf("ripemd160 word = %x, want padded %x", got.Ret, want)
	}
}

// TestPrecompileGas pins each precompile's price and its out-of-gas behaviour.
func TestPrecompileGas(t *testing.T) {
	word32 := make([]byte, 32) // exactly one word of input

	cases := []struct {
		addr byte
		cost uint64
	}{
		{4, 15 + 3},    // identity: 15 + 3/word
		{2, 60 + 12},   // sha256:   60 + 12/word
		{3, 600 + 120}, // ripemd160: 600 + 120/word
	}
	for _, c := range cases {
		p := precompileFor(addr(c.addr))
		if got := p.gas(word32); got != c.cost {
			t.Errorf("%s gas for 1 word = %d, want %d", p.name, got, c.cost)
		}
		// One gas short of the price must be a hard out-of-gas.
		if r := runPrecompile(p, word32, c.cost-1); r.Err != ErrOutOfGas {
			t.Errorf("%s underfunded: got err %v, want ErrOutOfGas", p.name, r.Err)
		}
		// Exactly the price must succeed with zero left.
		if r := runPrecompile(p, word32, c.cost); r.Err != nil || r.GasLeft != 0 {
			t.Errorf("%s exact gas: err=%v left=%d, want nil/0", p.name, r.Err, r.GasLeft)
		}
	}

	// ecrecover is a flat 3000 regardless of (fixed-size) input.
	if got := precompileFor(addr(1)).gas(nil); got != 3000 {
		t.Errorf("ecrecover gas = %d, want 3000", got)
	}
}

// TestEcrecoverRoundTrip signs with a known key and confirms the precompile
// recovers that key's address.
func TestEcrecoverRoundTrip(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	digest := common.Keccak256([]byte("recover me"))
	sig, err := crypto.Sign(digest, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	out, _ := ecrecoverRun(evmSigInput(digest, sig))
	if len(out) != 32 {
		t.Fatalf("ecrecover returned %d bytes, want 32", len(out))
	}
	want := key.Address()
	if !bytes.Equal(out[12:], want[:]) {
		t.Fatalf("recovered %x, want signer %x", out[12:], want[:])
	}
}

// TestEcrecoverRejectsInvalid covers the two empty-return paths: a bad v byte,
// and a non-canonical high-s (malleated) signature. The latter is a deliberate
// divergence: mainnet accepts it, but low-s is a chain-wide invariant here.
func TestEcrecoverRejectsInvalid(t *testing.T) {
	key, _ := crypto.GenerateKey()
	digest := common.Keccak256([]byte("x"))
	sig, _ := crypto.Sign(digest, key) // [v|r|s], guaranteed low-s

	// Bad v (not 27/28) -> empty.
	badV := evmSigInput(digest, sig)
	badV[63] = 26
	if out, _ := ecrecoverRun(badV); out != nil {
		t.Errorf("v=26 should recover empty, got %x", out)
	}

	// Malleate to the high-s twin: s' = N - s, v flipped. Same signer on
	// mainnet, rejected here.
	secpN, _ := new(big.Int).SetString("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 16)
	s := new(big.Int).SetBytes(sig[33:65])
	highS := new(big.Int).Sub(secpN, s)
	malleated := make([]byte, 65)
	malleated[0] = sig[0] ^ 0x01 // flip 27<->28
	copy(malleated[1:33], sig[1:33])
	highS.FillBytes(malleated[33:65])
	if out, _ := ecrecoverRun(evmSigInput(digest, malleated)); out != nil {
		t.Errorf("high-s (malleated) signature must recover empty, got %x", out)
	}
}

// TestPrecompileViaCALL checks the wiring: bytecode that CALLs the identity
// precompile (0x04) and returns its output. Without opCall routing to the
// precompile, calling 0x04 (a code-less account) would return empty.
func TestPrecompileViaCALL(t *testing.T) {
	state := newMockState()
	payload := new(big.Int).SetBytes([]byte("hello-precompile"))

	code := append(push32(payload), // memory[0] = payload
		byte(PUSH1), 0x00, byte(MSTORE))
	code = append(code,
		byte(PUSH1), 0x20, // retSize 32
		byte(PUSH1), 0x20, // retOff 0x20
		byte(PUSH1), 0x20, // argsSize 32
		byte(PUSH1), 0x00, // argsOff 0
		byte(PUSH1), 0x00, // value 0
		byte(PUSH1), 0x04, // target = identity precompile
		byte(GAS),
		byte(CALL),
		byte(PUSH1), 0x20, byte(PUSH1), 0x20, byte(RETURN)) // return memory[0x20:0x40]

	ctx := Context{Address: addr(0xAA), State: state}
	r := Run(code, nil, 1_000_000, ctx)
	if r.Err != nil {
		t.Fatalf("execution failed: %v", r.Err)
	}
	var want [32]byte
	payload.FillBytes(want[:])
	if !bytes.Equal(r.Ret, want[:]) {
		t.Fatalf("CALL to identity returned %x, want %x", r.Ret, want)
	}
}

// evmSigInput builds the 128-byte ECRECOVER input from a digest and a compact
// [v|r|s] signature.
func evmSigInput(digest common.Hash, sig []byte) []byte {
	in := make([]byte, 128)
	copy(in[0:32], digest[:])
	in[63] = sig[0] // v
	copy(in[64:96], sig[1:33])
	copy(in[96:128], sig[33:65])
	return in
}
