package common

import (
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/sha3"
)

const (
	HashLength    = 32
	AddressLength = 20
)

// Hash is a 32-byte keccak256 digest.
type Hash [HashLength]byte

// Address is the last 20 bytes of keccak256(pubkey[1:]).
type Address [AddressLength]byte

var ZeroHash = Hash{}
var ZeroAddress = Address{}

// Keccak256 is the chain's hash function. Every commitment (tx, block, merkle,
// state roots) goes through here, in one place so it is one line to swap.
func Keccak256(chunks ...[]byte) Hash {
	h := sha3.NewLegacyKeccak256()
	for _, c := range chunks {
		h.Write(c)
	}
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

func (h Hash) Bytes() []byte  { return h[:] }
func (h Hash) Hex() string    { return "0x" + hex.EncodeToString(h[:]) }
func (h Hash) String() string { return h.Hex() }
func (h Hash) IsZero() bool   { return h == ZeroHash }

func (a Address) Bytes() []byte  { return a[:] }
func (a Address) Hex() string    { return "0x" + hex.EncodeToString(a[:]) }
func (a Address) String() string { return a.Hex() }
func (a Address) IsZero() bool   { return a == ZeroAddress }

func HashFromBytes(b []byte) (Hash, error) {
	var h Hash
	if len(b) != HashLength {
		return h, errors.New("types: bad hash length")
	}
	copy(h[:], b)
	return h, nil
}

func AddressFromBytes(b []byte) (Address, error) {
	var a Address
	if len(b) != AddressLength {
		return a, errors.New("types: bad address length")
	}
	copy(a[:], b)
	return a, nil
}

func AddressFromHex(s string) (Address, error) {
	var a Address
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return a, err
	}
	return AddressFromBytes(b)
}

func HashFromHex(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return h, err
	}
	return HashFromBytes(b)
}

// MarshalText/UnmarshalText give 0x-prefixed hex in JSON, used by the RPC layer.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.Hex()), nil }
func (h *Hash) UnmarshalText(t []byte) error {
	v, err := HashFromHex(string(t))
	if err != nil {
		return err
	}
	*h = v
	return nil
}

func (a Address) MarshalText() ([]byte, error) { return []byte(a.Hex()), nil }
func (a *Address) UnmarshalText(t []byte) error {
	v, err := AddressFromHex(string(t))
	if err != nil {
		return err
	}
	*a = v
	return nil
}
