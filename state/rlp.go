package state

import "encoding/binary"

// Minimal RLP encoding, enough to derive a contract address the way Ethereum
// does: keccak256(rlp([sender, nonce]))[12:]. Not a general codec (no decoding,
// one level of list, no big integers). A one-byte difference from Ethereum's
// encoding yields a different address than external tooling computes.
//
// RLP reference: Ethereum Yellow Paper, Appendix B. The cases used here:
//   - a byte in [0x00,0x7f] encodes as itself,
//   - a string of length n<56 encodes as (0x80+n) then the bytes,
//   - a list with payload length n<56 encodes as (0xc0+n) then the items.
// The long-form (>=56) branches are implemented but unreached by [address, nonce].

// rlpBytes encodes a byte string.
func rlpBytes(b []byte) []byte {
	// A lone byte below 0x80 is its own RLP, no prefix; getting this wrong shifts
	// every low-nonce contract address.
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(rlpLength(len(b), 0x80), b...)
}

// rlpUint encodes an unsigned integer as a minimal big-endian string: no leading
// zeros, and zero is the empty string (0x80), not a literal 0x00. A leading zero
// would change the hash and the derived address.
func rlpUint(x uint64) []byte {
	if x == 0 {
		return rlpBytes(nil) // empty string -> 0x80
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], x)
	i := 0
	for buf[i] == 0 { // strip leading zeros; x!=0 guarantees a non-zero byte exists
		i++
	}
	return rlpBytes(buf[i:])
}

// rlpList wraps already-encoded items in a single list header.
func rlpList(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	return append(rlpLength(len(payload), 0xc0), payload...)
}

// rlpLength builds the length prefix. offset is 0x80 for strings, 0xc0 for
// lists. n<56 is the common short form; the long form encodes the length
// itself as big-endian bytes and prefixes their count.
func rlpLength(n int, offset byte) []byte {
	if n < 56 {
		return []byte{offset + byte(n)}
	}
	var lb [8]byte
	binary.BigEndian.PutUint64(lb[:], uint64(n))
	i := 0
	for lb[i] == 0 {
		i++
	}
	lenBytes := lb[i:]
	return append([]byte{offset + 55 + byte(len(lenBytes))}, lenBytes...)
}
