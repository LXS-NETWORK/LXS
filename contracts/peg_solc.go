package contracts

import (
	_ "embed"
	"math/big"

	"lxs/common"
)

// The LXS<->Base custodial peg, compiled from solidity/Peg.sol (solc 0.8.26,
// evmVersion=istanbul) and embedded so go test needs no toolchain. PegVault runs
// on LXS (locks native LXS backing); WrappedLXS is the ERC-20 minted on Base
// against it. See Peg.sol for the trust model and the reserve() >=
// wLXS.totalSupply() invariant the operator maintains.

//go:embed solidity/PegVault.bin
var pegVaultHex string

//go:embed solidity/WrappedLXS.bin
var wrappedLXSHex string

// PegVaultInit builds the deploy bytecode for PegVault(operator).
func PegVaultInit(operator common.Address) []byte {
	return append(decodeHex("PegVault", pegVaultHex), addrWord(operator)...)
}

// WrappedLXSInit builds the deploy bytecode for WrappedLXS(operator).
func WrappedLXSInit(operator common.Address) []byte {
	return append(decodeHex("WrappedLXS", wrappedLXSHex), addrWord(operator)...)
}

// --- PegVault calldata (runs on LXS) ---
func PegLockCalldata() []byte    { return []byte{0xf8, 0x3d, 0x08, 0xba} } // lock()
func PegReserveCalldata() []byte { return []byte{0xcd, 0x32, 0x93, 0xde} } // reserve()

// PegReleaseCalldata builds release(nonce, to, amount). nonce is the redeem event's
// nonce; the vault rejects a repeat, so a relayer cannot release the same redeem twice.
func PegReleaseCalldata(nonce *big.Int, to common.Address, amount *big.Int) []byte {
	d := append([]byte{0x22, 0x9c, 0x9f, 0x6c}, uint256Word(nonce)...) // release(uint256,address,uint256)
	d = append(d, addrWord(to)...)
	return append(d, uint256Word(amount)...)
}

// PegLockedTopic is topic0 of Locked(uint256,address,uint256), for the watcher's
// eth_getLogs filter on the LXS side.
func PegLockedTopic() common.Hash { return common.Keccak256([]byte("Locked(uint256,address,uint256)")) }

// --- WrappedLXS calldata (the ERC-20 on Base) ---

// WlxsMintCalldata builds mint(nonce, to, amount). nonce is the lock event's nonce;
// the token rejects a repeat, so a lock can never be double-minted.
func WlxsMintCalldata(nonce *big.Int, to common.Address, amount *big.Int) []byte {
	d := append([]byte{0x83, 0x6a, 0x10, 0x40}, uint256Word(nonce)...) // mint(uint256,address,uint256)
	d = append(d, addrWord(to)...)
	return append(d, uint256Word(amount)...)
}
func WlxsRedeemCalldata(amount *big.Int) []byte {
	return append([]byte{0xdb, 0x00, 0x6a, 0x75}, uint256Word(amount)...) // redeem(uint256)
}

// WlxsSetTokenURICalldata builds setTokenURI(uri) — installs the ERC-1046 logo/metadata
// data: URI once (operator-only, then frozen), so wallets show the LXS logo automatically.
func WlxsSetTokenURICalldata(uri string) []byte {
	d := append([]byte{0xe0, 0xdf, 0x5b, 0x6f}, uint256Word(big.NewInt(0x20))...) // setTokenURI(string)
	return append(d, abiString(uri)...)
}

// WlxsTokenURICalldata reads tokenURI() (ERC-1046).
func WlxsTokenURICalldata() []byte { return []byte{0x3c, 0x13, 0x0d, 0x90} }

// WlxsRedeemTopic is topic0 of Redeem(uint256,address,uint256), for the watcher's
// eth_getLogs filter on the Base side.
func WlxsRedeemTopic() common.Hash {
	return common.Keccak256([]byte("Redeem(uint256,address,uint256)"))
}

// balanceOf / totalSupply / transfer on wLXS reuse the shared ERC-20 calldata
// (BalanceOfCalldata, TotalSupplyCalldata, TransferCalldata) — identical selectors.
