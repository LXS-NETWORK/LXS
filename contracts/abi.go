package contracts

import (
	"encoding/hex"
	"math/big"
	"strings"

	"lxs/common"
)

// ABI-encoding helpers shared by the embedded-bytecode drivers
// (usertoken_solc.go, bondingcurve_solc.go): constructor args and calldata as
// 32-byte EVM words. Not a general ABI library, only what the factories need.

// decodeHex decodes an embedded hex constant. Panics (with a label) on bad input:
// these are embedded assets, so a decode failure is a build bug that must fail
// loudly at startup, not a runtime error.
func decodeHex(label, s string) []byte {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("contracts: bad embedded hex for " + label + ": " + err.Error())
	}
	return b
}

// uint256Word left-pads x into a 32-byte big-endian ABI word. x must be
// non-negative and fit in 256 bits (FillBytes panics otherwise) — true for every
// supply/amount/offset the factories encode.
func uint256Word(x *big.Int) []byte {
	w := make([]byte, 32)
	return x.FillBytes(w)
}

// addrWord left-pads a 20-byte address into a 32-byte ABI word (12 zero bytes
// then the address), the ABI encoding of an `address` argument.
func addrWord(a common.Address) []byte {
	w := make([]byte, 32)
	copy(w[32-len(a):], a[:])
	return w
}
