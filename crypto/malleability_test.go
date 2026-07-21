package crypto

import (
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"lxs/common"
)

// malleate returns the other half of the signature pair: same key, same message,
// different bytes. Needs no private key, only a signature seen on the wire.
func malleate(sig []byte) [][]byte {
	n := secp256k1.S256().N
	s := new(big.Int).SetBytes(sig[33:65])
	flipped := new(big.Int).Sub(n, s)

	base := make([]byte, SignatureLength)
	copy(base, sig)
	sb := flipped.Bytes()
	// Left-pad to 32 bytes.
	for i := 33; i < 65; i++ {
		base[i] = 0
	}
	copy(base[65-len(sb):65], sb)

	var out [][]byte
	for _, v := range []byte{sig[0] ^ 1, sig[0] + 1, sig[0] - 1} {
		c := make([]byte, SignatureLength)
		copy(c, base)
		c[0] = v
		out = append(out, c)
	}
	return out
}

// The signer must never produce a high-s signature.
func TestSignProducesCanonicalLowS(t *testing.T) {
	digest := common.Keccak256([]byte("canonical"))
	for i := 0; i < 200; i++ {
		k, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		sig, err := Sign(digest, k)
		if err != nil {
			t.Fatal(err)
		}
		s := new(big.Int).SetBytes(sig[33:65])
		if s.Cmp(halfOrder) > 0 {
			t.Fatalf("iteration %d: signer produced a high-s signature", i)
		}
	}
}

// Producing low-s in the signer is not a defence: an attacker uses their own.
// Only verification can enforce it.
func TestMalleatedSignatureIsRejected(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	digest := common.Keccak256([]byte("pay bob 100"))
	sig, err := Sign(digest, k)
	if err != nil {
		t.Fatal(err)
	}

	want, err := RecoverAddress(digest, sig)
	if err != nil {
		t.Fatal(err)
	}
	if want != k.Address() {
		t.Fatal("the honest signature does not recover to the signer")
	}

	// Every malleated variant must be rejected. One recovering to the same
	// address means the same transaction has two valid encodings, two hashes.
	for i, m := range malleate(sig) {
		got, err := RecoverAddress(digest, m)
		if err == nil && got == want {
			t.Fatalf("variant %d: a malleated signature recovered to the same sender %s — "+
				"the same transaction now has two hashes", i, got.Hex())
		}
		if err != ErrHighS && err == nil {
			t.Fatalf("variant %d: expected ErrHighS, got a different sender %s", i, got.Hex())
		}
	}
}

// The rejection must come from the low-s rule specifically, not from the
// signature happening to fail for some other reason.
func TestHighSIsRejectedWithTheRightError(t *testing.T) {
	k, _ := GenerateKey()
	digest := common.Keccak256([]byte("x"))
	sig, _ := Sign(digest, k)

	high := make([]byte, SignatureLength)
	copy(high, sig)
	n := secp256k1.S256().N
	s := new(big.Int).SetBytes(sig[33:65])
	flipped := new(big.Int).Sub(n, s)
	for i := 33; i < 65; i++ {
		high[i] = 0
	}
	fb := flipped.Bytes()
	copy(high[65-len(fb):65], fb)

	if _, err := Recover(digest, high); err != ErrHighS {
		t.Fatalf("high-s signature: got %v want ErrHighS", err)
	}
}

// s == N/2 is canonical and must be accepted. An off-by-one would reject a real
// fraction of honest signatures, looking like random wallet failures.
func TestBoundaryOfLowSIsInclusive(t *testing.T) {
	sig := make([]byte, SignatureLength)
	sig[0] = 27
	sig[1] = 1 // non-zero r, so only the s rule is under test

	hb := halfOrder.Bytes()
	copy(sig[65-len(hb):65], hb)
	if err := checkLowS(sig); err != nil {
		t.Fatalf("s == N/2 must be accepted, got %v", err)
	}

	over := new(big.Int).Add(halfOrder, big.NewInt(1))
	ob := over.Bytes()
	for i := 33; i < 65; i++ {
		sig[i] = 0
	}
	copy(sig[65-len(ob):65], ob)
	if err := checkLowS(sig); err != ErrHighS {
		t.Fatalf("s == N/2+1 must be rejected, got %v", err)
	}
}

func TestBadSignatureLengthIsRejected(t *testing.T) {
	digest := common.Keccak256([]byte("x"))
	for _, n := range []int{0, 1, 64, 66, 128} {
		if _, err := Recover(digest, make([]byte, n)); err == nil {
			t.Fatalf("a %d-byte signature was accepted", n)
		}
	}
}
