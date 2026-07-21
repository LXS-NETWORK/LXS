package rlp

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestEncodeKnownVectors pins the encoder against the canonical examples in the
// RLP spec / Ethereum wiki.
func TestEncodeKnownVectors(t *testing.T) {
	cases := []struct {
		got  []byte
		want string
	}{
		{Bytes(nil), "80"},                    // empty string
		{Bytes([]byte("dog")), "83646f67"},    // "dog"
		{Bytes([]byte{0x00}), "00"},           // single zero byte is itself
		{Bytes([]byte{0x0f}), "0f"},           // single byte < 0x80
		{Bytes([]byte{0x04, 0x00}), "820400"}, // two bytes
		{Uint(0), "80"},                       // 0 -> empty string
		{Uint(15), "0f"},                      // 15
		{Uint(1024), "820400"},                // 1024
		{List(), "c0"},                        // empty list
		{List(Bytes([]byte("cat")), Bytes([]byte("dog"))), "c88363617483646f67"},
	}
	for i, c := range cases {
		if hex.EncodeToString(c.got) != c.want {
			t.Errorf("case %d: got %x, want %s", i, c.got, c.want)
		}
	}
}

// TestDecodeRoundTrip encodes a list of strings and decodes it back.
func TestDecodeRoundTrip(t *testing.T) {
	items := [][]byte{[]byte("cat"), []byte("dog"), {0x00}, {}, {0xde, 0xad, 0xbe, 0xef}}
	enc := make([][]byte, len(items))
	for i, it := range items {
		enc[i] = Bytes(it)
	}
	encoded := List(enc...)

	out, err := DecodeList(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(items) {
		t.Fatalf("decoded %d items, want %d", len(out), len(items))
	}
	for i := range items {
		if !bytes.Equal(out[i], items[i]) {
			t.Errorf("item %d: got %x, want %x", i, out[i], items[i])
		}
	}
}

// TestDecodeRejectsMalformed checks the decoder never trusts a length: each of
// these is a bounds or canonicalization attack that must error, not panic. This
// is the eth_sendRawTransaction security surface.
func TestDecodeRejectsMalformed(t *testing.T) {
	bad := []string{
		"c1",     // list claims 1 payload byte, none present
		"83646f", // string claims 3 bytes, only 2 present
		"b800",   // long string, length byte is a leading zero
		"8100",   // 0x81 0x00 — non-canonical (should be the bare byte 0x00)
		"c000",   // trailing byte after a complete list
		"f8",     // long list header with no length byte
	}
	for _, h := range bad {
		data, err := hex.DecodeString(clean(h))
		if err != nil {
			continue
		}
		if _, err := DecodeList(data); err == nil {
			t.Errorf("malformed input %q was accepted", h)
		}
	}
}

func clean(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// A crafted 8-byte length prefix must ERROR, never panic. 0xff = long list with an
// 8-byte length; 0x7fffffffffffffff overflows a signed int, which used to wrap
// start+n negative so the bounds check passed and the slice panicked
// (slice bounds out of range [:-9223372036854775800]). The package guarantees
// malformed RLP returns an error, never a panic or out-of-bounds read — reachable
// from eth_sendRawTransaction, so on an immutable chain the guarantee must hold.
func TestDecodeHugeLengthPrefixDoesNotPanic(t *testing.T) {
	cases := [][]byte{
		{0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // long list, ~2^63 length
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // long list, 2^64-1 length
		{0xbf, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // long string, ~2^63 length
	}
	for _, data := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("DecodeList(%x) panicked instead of erroring: %v", data, r)
				}
			}()
			if _, err := DecodeList(data); err == nil {
				t.Fatalf("DecodeList(%x) accepted an impossible length, want an error", data)
			}
		}()
	}
}
