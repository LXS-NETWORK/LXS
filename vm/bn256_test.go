package vm

import (
	"bytes"
	"encoding/hex"
	"testing"

	"lxs/common"
)

func hexb(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// Vectors are go-ethereum's core/vm/testdata/precompiles/{bn256Add,bn256ScalarMul,
// bn256Pairing}.json. Passing them proves both the alt_bn128 arithmetic AND the
// Ethereum point encoding (64-byte G1 / 128-byte G2, no tag byte) match the
// reference bit-for-bit — a wrong curve or byte order fails here rather than
// diverging live.
func TestBn256AddVector(t *testing.T) {
	in := hexb(t, "18b18acfb4c2c30276db5411368e7185b311dd124691610c5d3b74034e093dc9063c909c4720840cb5134cb9f59fa749755796819658d32efc0d288198f3726607c2b7f58a84bd6145f00c9c2bc0bb1a187f20ff2c92963a88019e7c6a014eed06614e20c147e940f2d70da3f74c9a17df361706a4485c742bd6788478fa17d7")
	want := hexb(t, "2243525c5efd4b9c3d3c45ac0ca3fe4dd85e830a4ce6b65fa1eeaee202839703301d1d33be6da8e509df21cc35964723180eed7532537db9ae5e7d48f195c915")
	got, err := bn256AddPrecompile.run(in)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("bn256Add = %x, err=%v; want %x", got, err, want)
	}
	if g := bn256AddPrecompile.gas(in); g != 150 {
		t.Fatalf("add gas = %d, want 150 (EIP-1108)", g)
	}
}

func TestBn256ScalarMulVector(t *testing.T) {
	in := hexb(t, "2bd3e6d0f3b142924f5ca7b49ce5b9d54c4703d7ae5648e61d02268b1a0a9fb721611ce0a6af85915e2f1d70300909ce2e49dfad4a4619c8390cae66cefdb20400000000000000000000000000000000000000000000000011138ce750fa15c2")
	want := hexb(t, "070a8d6a982153cae4be29d434e8faef8a47b274a053f5a4ee2a6c9c13c31e5c031b8ce914eba3a9ffb989f9cdd5b0f01943074bf4f0f315690ec3cec6981afc")
	got, err := bn256ScalarMulPrecompile.run(in)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("bn256ScalarMul = %x, err=%v; want %x", got, err, want)
	}
	if g := bn256ScalarMulPrecompile.gas(in); g != 6000 {
		t.Fatalf("mul gas = %d, want 6000 (EIP-1108)", g)
	}
}

func TestBn256PairingVectors(t *testing.T) {
	jeff1 := hexb(t, "1c76476f4def4bb94541d57ebba1193381ffa7aa76ada664dd31c16024c43f593034dd2920f673e204fee2811c678745fc819b55d3e9d294e45c9b03a76aef41209dd15ebff5d46c4bd888e51a93cf99a7329636c63514396b4a452003a35bf704bf11ca01483bfa8b34b43561848d28905960114c8ac04049af4b6315a416782bb8324af6cfc93537a2ad1a445cfd0ca2a71acd7ac41fadbf933c2a51be344d120a2a4cf30c1bf9845f20c6fe39e07ea2cce61f0c9bb048165fe5e4de877550111e129f1cf1097710d41c4ac70fcdfa5ba2023c6ff1cbeac322de49d1b6df7c2032c61a830e3c17286de9462bf242fca2883585b93870a73853face6a6bf411198e9393920d483a7260bfb731fb5d25f1aa493335a9e71297e485b7aef312c21800deef121f1e76426a00665e5c4479674322d4f75edadd46debd5cd992f6ed090689d0585ff075ec9e99ad690c3395bc4b313370b38ef355acdadcd122975b12c85ea5db8c6deb4aab71808dcb408fe3d1e7690c43d37b4ce6cc0166fa7daa")
	// TRUE
	if got, err := bn256PairingPrecompile.run(jeff1); err != nil || got[31] != 1 {
		t.Fatalf("jeff1 pairing = %x err=%v, want ...01 (true)", got, err)
	}
	// gas: 384 bytes = 2 pairs -> 45000 + 2*34000
	if g := bn256PairingPrecompile.gas(jeff1); g != 45000+2*34000 {
		t.Fatalf("pairing gas = %d, want %d", g, 45000+2*34000)
	}

	jeff6 := hexb(t, "1c76476f4def4bb94541d57ebba1193381ffa7aa76ada664dd31c16024c43f593034dd2920f673e204fee2811c678745fc819b55d3e9d294e45c9b03a76aef41209dd15ebff5d46c4bd888e51a93cf99a7329636c63514396b4a452003a35bf704bf11ca01483bfa8b34b43561848d28905960114c8ac04049af4b6315a416782bb8324af6cfc93537a2ad1a445cfd0ca2a71acd7ac41fadbf933c2a51be344d120a2a4cf30c1bf9845f20c6fe39e07ea2cce61f0c9bb048165fe5e4de877550111e129f1cf1097710d41c4ac70fcdfa5ba2023c6ff1cbeac322de49d1b6df7c103188585e2364128fe25c70558f1560f4f9350baf3959e603cc91486e110936198e9393920d483a7260bfb731fb5d25f1aa493335a9e71297e485b7aef312c21800deef121f1e76426a00665e5c4479674322d4f75edadd46debd5cd992f6ed090689d0585ff075ec9e99ad690c3395bc4b313370b38ef355acdadcd122975b12c85ea5db8c6deb4aab71808dcb408fe3d1e7690c43d37b4ce6cc0166fa7daa")
	// FALSE
	if got, err := bn256PairingPrecompile.run(jeff6); err != nil || got[31] != 0 {
		t.Fatalf("jeff6 pairing = %x err=%v, want ...00 (false)", got, err)
	}
}

// Empty input is the empty product = 1 (true), and its base gas is still charged.
func TestBn256PairingEmptyIsTrue(t *testing.T) {
	got, err := bn256PairingPrecompile.run(nil)
	if err != nil || len(got) != 32 || got[31] != 1 {
		t.Fatalf("empty pairing = %x err=%v, want 32-byte ...01", got, err)
	}
	if g := bn256PairingPrecompile.gas(nil); g != 45000 {
		t.Fatalf("empty pairing gas = %d, want base 45000", g)
	}
}

// A length that is not a multiple of 192 must FAIL the call (EIP-197), so a
// truncated trailing pair can't be silently zero-extended into a different check.
func TestBn256PairingBadLengthFails(t *testing.T) {
	if _, err := bn256PairingPrecompile.run(make([]byte, 100)); err == nil {
		t.Fatal("pairing with a non-192-multiple input must fail the call")
	}
}

// THE LANDMINE THIS ITEM FIXES: a malformed / off-curve point must FAIL the call,
// never return empty+success. Point (1,1) is not on y^2 = x^3 + 3, so it must be
// rejected — otherwise a verifier reads a forged result as valid.
func TestBn256MalformedPointFailsCall(t *testing.T) {
	bad := make([]byte, 128) // Add takes two G1 points
	bad[31] = 1              // x0 = 1
	bad[63] = 1              // y0 = 1  -> (1,1), off-curve
	if _, err := bn256AddPrecompile.run(bad); err == nil {
		t.Fatal("off-curve point must fail the call, not return empty+success (the forgery landmine)")
	}

	// And through runPrecompile the failure surfaces as a call fault with all gas
	// consumed, exactly like an EVM hard fault.
	res := runPrecompile(&bn256AddPrecompile, bad, 1_000_000)
	if res.Err == nil || res.GasLeft != 0 {
		t.Fatalf("malformed-point call: err=%v gasLeft=%d, want an error and 0 gas left", res.Err, res.GasLeft)
	}
}

// 0x06/0x07/0x08 must resolve to a precompile — before this item they hit no
// precompile and a CALL silently succeeded with empty output.
func TestBn256AddressesWired(t *testing.T) {
	for _, last := range []byte{6, 7, 8} {
		var a common.Address
		a[len(a)-1] = last
		if precompileFor(a) == nil {
			t.Fatalf("0x0%d resolves to no precompile — the silent-empty landmine", last)
		}
	}
}
