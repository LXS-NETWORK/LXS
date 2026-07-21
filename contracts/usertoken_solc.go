package contracts

import (
	_ "embed"
	"math/big"
)

// UserToken is the fixed-supply ERC-20 primitive: name, symbol, whole supply
// minted to the deployer. Compiled from solidity/UserToken.sol (solc/istanbul)
// and checked in as bytecode; the create-token CLI deploys it in one step.
//
//go:embed solidity/UserToken.bin
var userTokenHex string

// UserTokenInit is the raw creation bytecode (constructor args NOT yet appended).
func UserTokenInit() []byte { return decodeHex("UserToken", userTokenHex) }

// UserTokenDeploy is the full deploy bytecode: creation code with the constructor
// args (name, symbol, supply) ABI-encoded and appended, the tail layout solc
// reads them from. supply is in wei (scaled by 1e18).
func UserTokenDeploy(name, symbol string, supply *big.Int) []byte {
	encodeString := func(s string) []byte {
		out := uint256Word(big.NewInt(int64(len(s))))
		b := []byte(s)
		out = append(out, b...)
		if pad := (32 - len(b)%32) % 32; pad > 0 {
			out = append(out, make([]byte, pad)...)
		}
		return out
	}
	nameEnc := encodeString(name)
	symEnc := encodeString(symbol)

	// head: offset(name), offset(symbol), supply — 3 words = 0x60.
	head := make([]byte, 0, 96)
	head = append(head, uint256Word(big.NewInt(0x60))...)
	head = append(head, uint256Word(big.NewInt(int64(0x60+len(nameEnc))))...)
	head = append(head, uint256Word(supply)...)

	args := append(head, nameEnc...)
	args = append(args, symEnc...)
	return append(UserTokenInit(), args...)
}

// NameCalldata / SymbolCalldata read a token's name()/symbol() (both return a
// dynamic string; callers ABI-decode the offset+length+bytes).
func NameCalldata() []byte   { return []byte{0x06, 0xfd, 0xde, 0x03} }
func SymbolCalldata() []byte { return []byte{0x95, 0xd8, 0x9b, 0x41} }

// TotalSupplyCalldata reads totalSupply() (selector keccak256("totalSupply()")[:4]).
func TotalSupplyCalldata() []byte { return []byte{0x18, 0x16, 0x0d, 0xdd} }
