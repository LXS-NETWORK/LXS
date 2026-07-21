// Package rlp implements enough of Ethereum's Recursive Length Prefix encoding
// to encode and decode a legacy transaction, the wire format MetaMask and other
// wallets speak.
//
// The decoder runs on attacker-controlled bytes (an eth_sendRawTransaction
// body). Every length is bounds-checked against the remaining input before
// slicing, so malformed RLP returns an error, never a panic or out-of-bounds
// read.
package rlp

import (
	"errors"
	"math/big"
)

var (
	ErrShortInput  = errors.New("rlp: input shorter than its declared length")
	ErrLeadingZero = errors.New("rlp: length prefix has a leading zero")
	ErrTrailing    = errors.New("rlp: trailing bytes after the top-level item")
	ErrNested      = errors.New("rlp: unexpected nested list")
	ErrExpectList  = errors.New("rlp: expected a list at the top level")
	ErrCanon       = errors.New("rlp: non-canonical encoding")
)

// --- encoding ---

// Bytes encodes a byte string.
func Bytes(b []byte) []byte {
	// A single byte below 0x80 is its own encoding.
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(lengthPrefix(len(b), 0x80), b...)
}

// Uint encodes an unsigned integer as a minimal big-endian byte string (no
// leading zeros; zero is the empty string).
func Uint(x uint64) []byte { return Bytes(trimLeadingZeros(bigEndian(x))) }

// Big encodes a non-negative big.Int the same way.
func Big(x *big.Int) []byte {
	if x == nil || x.Sign() == 0 {
		return Bytes(nil)
	}
	return Bytes(x.Bytes()) // big.Int.Bytes() is already minimal big-endian
}

// List wraps already-encoded items in a list header.
func List(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	return append(lengthPrefix(len(payload), 0xc0), payload...)
}

func lengthPrefix(n int, offset byte) []byte {
	if n < 56 {
		return []byte{offset + byte(n)}
	}
	lb := trimLeadingZeros(bigEndian(uint64(n)))
	return append([]byte{offset + 55 + byte(len(lb))}, lb...)
}

func bigEndian(x uint64) []byte {
	return []byte{byte(x >> 56), byte(x >> 48), byte(x >> 40), byte(x >> 32),
		byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)}
}

func trimLeadingZeros(b []byte) []byte {
	i := 0
	for i < len(b) && b[i] == 0 {
		i++
	}
	return b[i:]
}

// --- decoding ---

// DecodeList decodes a top-level list of byte strings (the shape of a legacy
// transaction). A nested list, trailing garbage, or an overrunning length is an
// error, not a crash.
func DecodeList(data []byte) ([][]byte, error) {
	payload, isList, rest, err := readItem(data)
	if err != nil {
		return nil, err
	}
	if !isList {
		return nil, ErrExpectList
	}
	if len(rest) != 0 {
		return nil, ErrTrailing
	}
	var out [][]byte
	for len(payload) > 0 {
		content, elemIsList, r, err := readItem(payload)
		if err != nil {
			return nil, err
		}
		if elemIsList {
			return nil, ErrNested
		}
		out = append(out, content)
		payload = r
	}
	return out, nil
}

// readItem parses one RLP item at the front of data, returning its content,
// whether it was a list, and the bytes that follow. Bounds are checked before
// every slice.
func readItem(data []byte) (content []byte, isList bool, rest []byte, err error) {
	if len(data) == 0 {
		return nil, false, nil, ErrShortInput
	}
	b := data[0]
	switch {
	case b < 0x80: // single byte, its own encoding
		return data[:1], false, data[1:], nil

	case b < 0xb8: // short string, 0..55 bytes
		n := int(b - 0x80)
		if len(data) < 1+n {
			return nil, false, nil, ErrShortInput
		}
		s := data[1 : 1+n]
		// A single byte < 0x80 must use the compact form, not 0x81<byte>.
		if n == 1 && s[0] < 0x80 {
			return nil, false, nil, ErrCanon
		}
		return s, false, data[1+n:], nil

	case b < 0xc0: // long string
		nLen := int(b - 0xb7)
		n, err := readLen(data, nLen)
		if err != nil {
			return nil, false, nil, err
		}
		start := 1 + nLen
		if len(data) < start+n {
			return nil, false, nil, ErrShortInput
		}
		return data[start : start+n], false, data[start+n:], nil

	case b < 0xf8: // short list
		n := int(b - 0xc0)
		if len(data) < 1+n {
			return nil, false, nil, ErrShortInput
		}
		return data[1 : 1+n], true, data[1+n:], nil

	default: // long list
		nLen := int(b - 0xf7)
		n, err := readLen(data, nLen)
		if err != nil {
			return nil, false, nil, err
		}
		start := 1 + nLen
		if len(data) < start+n {
			return nil, false, nil, ErrShortInput
		}
		return data[start : start+n], true, data[start+n:], nil
	}
}

// readLen reads an nLen-byte big-endian length that begins at data[1], with the
// canonical-form checks the RLP spec requires (no leading zero, minimal form).
func readLen(data []byte, nLen int) (int, error) {
	if len(data) < 1+nLen {
		return 0, ErrShortInput
	}
	lb := data[1 : 1+nLen]
	if lb[0] == 0 {
		return 0, ErrLeadingZero
	}
	// Accumulate as uint64: nLen can be 8, and a crafted prefix like
	// 0x7FFFFFFFFFFFFFFF would overflow a signed int, wrapping `start+n` negative in
	// the caller so its bounds check passes and the slice panics. This package
	// promises malformed RLP errors, never a panic — so accumulate width-safely.
	var n uint64
	for _, c := range lb {
		n = n<<8 | uint64(c)
	}
	if n < 56 { // a length this small must have used the short form
		return 0, ErrCanon
	}
	// A declared length can never exceed the bytes actually present after the
	// prefix. Rejecting anything larger both catches truncated input and makes the
	// overflow above impossible: n is now bounded by len(data), so it fits in int
	// and start+n cannot wrap.
	if n > uint64(len(data)-(1+nLen)) {
		return 0, ErrShortInput
	}
	return int(n), nil
}
