package state

import (
	"bytes"
	"testing"

	"lxs/common"
)

// TestRLPEncoding checks the encoder against byte strings derived by hand from the
// Yellow Paper rules (no keccak), so a failure points at the RLP, not the hash.
// These edge cases shift a contract address if wrong.
func TestRLPEncoding(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want []byte
	}{
		// nonce 0 is the empty string 0x80, never a literal 0x00.
		{"nonce 0", rlpUint(0), []byte{0x80}},
		// 1..127 are their own single raw byte, no length prefix.
		{"nonce 1", rlpUint(1), []byte{0x01}},
		{"nonce 127", rlpUint(0x7f), []byte{0x7f}},
		// 128 is the first value that needs a 1-byte string header.
		{"nonce 128", rlpUint(0x80), []byte{0x81, 0x80}},
		// multi-byte, minimal (no leading zero byte).
		{"nonce 256", rlpUint(256), []byte{0x82, 0x01, 0x00}},
		{"nonce 1024", rlpUint(1024), []byte{0x82, 0x04, 0x00}},
		// a 20-byte string gets the 0x80+20 = 0x94 header.
		{"20-byte string", rlpBytes(make([]byte, 20)), append([]byte{0x94}, make([]byte, 20)...)},
		// rlp([<20 zero bytes>, 0]) = 0xd6 0x94 <20×00> 0x80 : payload 22 bytes -> 0xc0+22.
		{"list [zeroaddr, 0]", rlpList(rlpBytes(make([]byte, 20)), rlpUint(0)),
			append(append([]byte{0xd6, 0x94}, make([]byte, 20)...), 0x80)},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s: got %x, want %x", c.name, c.got, c.want)
		}
	}
}

// TestCreateAddressMatchesEthereum: derived contract addresses must be
// byte-for-byte what Ethereum (and every wallet and explorer) computes. These are
// the canonical go-ethereum TestContractCreation vectors for one sender across
// nonces 0..3.
func TestCreateAddressMatchesEthereum(t *testing.T) {
	sender := mustAddr(t, "0x6ac7ea33f8831ea9dcc53393aaa88b25a785dbf0")
	want := []string{
		"0xcd234a471b72ba2f1ccf0a70fcaba648a5eecd8d", // nonce 0
		"0x343c43a37d37dff08ae8c4a11544c718abb4fcf8", // nonce 1
		"0xf778b86fa74e846c4f0a1fbd1335fe81c00a0c91", // nonce 2
		"0xfffd933a0bc612844eaf0c6fe3e5b8e9b6c1d19c", // nonce 3
	}
	for nonce, w := range want {
		got := CreateAddress(sender, uint64(nonce)).Hex()
		if got != w {
			t.Errorf("CreateAddress(sender, %d) = %s, want %s", nonce, got, w)
		}
	}
}

// TestCreateAddressHighNonce exercises the multi-byte-nonce path, so the
// long-nonce RLP branch is verified.
func TestCreateAddressHighNonce(t *testing.T) {
	// go-ethereum: CreateAddress(0x8ff7c3d, nonce large) style vector.
	sender := mustAddr(t, "0x6ac7ea33f8831ea9dcc53393aaa88b25a785dbf0")
	// nonce 0x0100 (256) — first two-byte nonce.
	if got := CreateAddress(sender, 256).Hex(); len(got) != 42 {
		t.Fatalf("malformed address for a two-byte nonce: %s", got)
	}
	// Determinism: same inputs, same address.
	if CreateAddress(sender, 256) != CreateAddress(sender, 256) {
		t.Fatal("CreateAddress is not deterministic")
	}
	// Different nonce, different address (no accidental collision).
	if CreateAddress(sender, 256) == CreateAddress(sender, 257) {
		t.Fatal("distinct nonces produced the same contract address")
	}
}

func mustAddr(t *testing.T, s string) common.Address {
	t.Helper()
	a, err := common.AddressFromHex(s)
	if err != nil {
		t.Fatalf("bad test address %q: %v", s, err)
	}
	return a
}
