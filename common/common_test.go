package common

import (
	"bytes"
	"math/big"
	"testing"
)

// Keccak256 must be Ethereum's legacy Keccak, not NIST SHA3-256: they differ only
// in padding, so a swap compiles and silently changes every commitment (tx/block/
// state/receipt roots), forking the chain from itself and Ethereum tooling. The
// known-answer vector keccak256("") is the tripwire.
func TestKeccak256IsEthereumKeccakNotSHA3(t *testing.T) {
	const emptyKeccak = "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
	if got := Keccak256(nil).Hex(); got != emptyKeccak {
		t.Fatalf("keccak256(\"\") = %s\n     want %s\n(a SHA3-256 swap would land on 0xa7ffc6f8bf1ed766...)", got, emptyKeccak)
	}
	// Chunked writes must equal the concatenated single write: Keccak256(a, b) == Keccak256(ab).
	if Keccak256([]byte("ab"), []byte("cd")) != Keccak256([]byte("abcd")) {
		t.Fatal("chunked Keccak256 differs from concatenated — hashing is not associative over chunks")
	}
}

// The canonical Encoder must produce byte-identical encodings across nodes or
// hashes diverge and consensus is impossible. These pin its determinism and
// injectivity.
func TestEncoderIsDeterministicAndInjective(t *testing.T) {
	build := func() []byte {
		e := NewEncoder()
		e.Uint64(1)
		e.Bytes([]byte("hi"))
		e.BigInt(big.NewInt(256))
		return e.Done()
	}
	if !bytes.Equal(build(), build()) {
		t.Fatal("same input produced different bytes — encoding is not deterministic")
	}

	// Length-prefixed Bytes must defeat concatenation ambiguity: ("a","b") and
	// ("ab","") are different logical inputs and must not collide.
	ab := func(x, y string) []byte {
		e := NewEncoder()
		e.Bytes([]byte(x))
		e.Bytes([]byte(y))
		return e.Done()
	}
	if bytes.Equal(ab("a", "b"), ab("ab", "")) {
		t.Fatal("Bytes is not injective — a length prefix is missing")
	}

	// OptionalAddress present vs absent must not collide (presence flag).
	addr := Address{0xAA}
	present := func() []byte { e := NewEncoder(); e.OptionalAddress(&addr); return e.Done() }
	absent := func() []byte { e := NewEncoder(); e.OptionalAddress(nil); return e.Done() }
	if bytes.Equal(present(), absent()) {
		t.Fatal("OptionalAddress(nil) collides with a present address")
	}
	if got := absent(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("absent OptionalAddress must be a single 0 byte, got %x", got)
	}
}

// BigInt encoding is minimal big-endian (big.Int.Bytes()), so it is canonical but
// magnitude-only: nil and 0 collide, and a negative collides with its positive
// twin. Balances are non-negative by invariant, so this is safe; the test pins
// the contract so a caller does not hash a signed quantity here.
func TestEncoderBigIntIsMagnitudeOnly(t *testing.T) {
	enc := func(v *big.Int) []byte { e := NewEncoder(); e.BigInt(v); return e.Done() }
	if !bytes.Equal(enc(nil), enc(big.NewInt(0))) {
		t.Fatal("BigInt(nil) and BigInt(0) are expected to encode identically (both empty)")
	}
	if !bytes.Equal(enc(big.NewInt(5)), enc(big.NewInt(-5))) {
		t.Fatal("BigInt encodes magnitude only — this test pins that contract (do not hash signed values)")
	}
	// Minimal: 256 is 0x0100, no leading zero, length prefix 2.
	e := NewEncoder()
	e.BigInt(big.NewInt(256))
	got := e.Done()
	want := []byte{0, 0, 0, 0, 0, 0, 0, 2, 0x01, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("BigInt(256) = %x, want %x (8-byte len prefix + minimal be)", got, want)
	}
}

// BurnAddress is a consensus constant: the state transition recognises a send
// here as a burn. A drifted literal makes one node burn and another credit, a
// consensus split. Pin the exact bytes.
func TestBurnAddressIsPinned(t *testing.T) {
	const want = "0x000000000000000000000000000000000000dead"
	if got := BurnAddress.Hex(); got != want {
		t.Fatalf("BurnAddress = %s, want %s — a drift here splits consensus", got, want)
	}
}

// AddressFromHex round-trips and rejects malformed input rather than silently
// truncating or padding: a recipient parsing to the wrong bytes moves money to
// the wrong place.
func TestAddressFromHexRoundTripAndRejects(t *testing.T) {
	a := Address{0x29, 0x12, 0x4a, 0x86}
	back, err := AddressFromHex(a.Hex())
	if err != nil || back != a {
		t.Fatalf("round-trip failed: %v back=%x", err, back)
	}
	for _, bad := range []string{"", "0x1234", "0xzz", "0x" + "aa" /*1 byte*/, "0x" + repeat("aa", 21) /*21 bytes*/} {
		if _, err := AddressFromHex(bad); err == nil {
			t.Fatalf("AddressFromHex(%q) accepted malformed input", bad)
		}
	}
}

// Log.EncodeInto must be deterministic and injective in the topic count, since it
// feeds the receipt root a light client trusts without re-execution.
func TestLogEncodeIsInjectiveInTopicCount(t *testing.T) {
	enc := func(l *Log) []byte { e := NewEncoder(); l.EncodeInto(e); return e.Done() }
	one := &Log{Address: Address{1}, Topics: []Hash{{9}}, Data: []byte("x")}
	// A different topic count with the same address+data must not collide with
	// `one`; the explicit count guards this.
	two := &Log{Address: Address{1}, Topics: []Hash{{9}, {}}, Data: []byte("x")}
	if bytes.Equal(enc(one), enc(two)) {
		t.Fatal("logs with different topic counts collide — receipt root is forgeable")
	}
	if !bytes.Equal(enc(one), enc(one)) {
		t.Fatal("Log.EncodeInto is not deterministic")
	}
}

func TestDenominationOneLXS(t *testing.T) {
	// 10^18 exactly.
	want := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	if OneLXS.Cmp(want) != 0 {
		t.Fatalf("OneLXS = %s, want %s", OneLXS, want)
	}
	if LXS(3).Cmp(new(big.Int).Mul(big.NewInt(3), want)) != 0 {
		t.Fatal("LXS(3) != 3 * 10^18")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
