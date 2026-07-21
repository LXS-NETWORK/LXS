package contracts

import (
	_ "embed"
	"math/big"

	"lxs/common"
)

// Graduation contracts, compiled from solidity/Graduation.sol (solc 0.8.26,
// evmVersion=istanbul) and embedded so go test needs no toolchain. GraduationVault
// runs on LXS (locks the committed LXS + coin as backing); WrappedToken is the
// generalized ERC-20 minted on Base against it, then paired with wLXS in a Uniswap
// pool. See Graduation.sol for the trust model and the locked-backing invariant.

//go:embed solidity/WrappedToken.bin
var wrappedTokenHex string

//go:embed solidity/GraduationVault.bin
var graduationVaultHex string

// WrappedTokenInit builds deploy bytecode for WrappedToken(operator, name, symbol) —
// the Base-side ERC-20 for a graduated coin, with the coin's own name/symbol.
func WrappedTokenInit(operator common.Address, name, symbol string) []byte {
	nameEnc := abiString(name)
	symEnc := abiString(symbol)
	head := addrWord(operator)
	head = append(head, uint256Word(big.NewInt(0x60))...)                     // offset to name (3 head words)
	head = append(head, uint256Word(big.NewInt(int64(0x60+len(nameEnc))))...) // offset to symbol
	args := append(head, nameEnc...)
	args = append(args, symEnc...)
	return append(decodeHex("WrappedToken", wrappedTokenHex), args...)
}

// GraduationVaultInit builds deploy bytecode for GraduationVault(operator, minLiquidity).
// minLiquidity is the on-chain "at least ~1 pound of LXS" commitment gate, in wei.
func GraduationVaultInit(operator common.Address, minLiquidity *big.Int) []byte {
	args := append(addrWord(operator), uint256Word(minLiquidity)...)
	return append(decodeHex("GraduationVault", graduationVaultHex), args...)
}

// abiString encodes a dynamic-string tail: a length word then the right-padded bytes.
func abiString(s string) []byte {
	b := []byte(s)
	out := uint256Word(big.NewInt(int64(len(b))))
	out = append(out, b...)
	if pad := (32 - len(b)%32) % 32; pad > 0 {
		out = append(out, make([]byte, pad)...)
	}
	return out
}

// --- GraduationVault calldata (runs on LXS) ---

// GraduateCalldata builds graduate(coin, tokenAmount). The committed LXS is the tx value.
func GraduateCalldata(coin common.Address, tokenAmount *big.Int) []byte {
	d := append([]byte{0xec, 0x8d, 0xc7, 0x54}, addrWord(coin)...) // graduate(address,uint256)
	return append(d, uint256Word(tokenAmount)...)
}
func GradLxsReserveCalldata() []byte { return []byte{0x4d, 0x34, 0xb0, 0xb7} } // lxsReserve()
func GradTokenReserveCalldata(coin common.Address) []byte {
	return append([]byte{0x54, 0xd3, 0x90, 0x08}, addrWord(coin)...) // tokenReserve(address)
}

// GradReleaseLxsCalldata builds releaseLxs(nonce, to, amount); the vault rejects a
// repeated nonce, so a relayer cannot release the same graduation's LXS twice.
func GradReleaseLxsCalldata(nonce *big.Int, to common.Address, amount *big.Int) []byte {
	d := append([]byte{0x45, 0xdf, 0xcd, 0xbb}, uint256Word(nonce)...) // releaseLxs(uint256,address,uint256)
	d = append(d, addrWord(to)...)
	return append(d, uint256Word(amount)...)
}

// GradReleaseTokenCalldata builds releaseToken(nonce, coin, to, amount).
func GradReleaseTokenCalldata(nonce *big.Int, coin, to common.Address, amount *big.Int) []byte {
	d := append([]byte{0x05, 0x5c, 0x48, 0x5a}, uint256Word(nonce)...) // releaseToken(uint256,address,address,uint256)
	d = append(d, addrWord(coin)...)
	d = append(d, addrWord(to)...)
	return append(d, uint256Word(amount)...)
}

// GraduatedTopic is topic0 of Graduated(uint256,address,address,uint256,uint256): the
// operator watches it to know which coin to bridge + pool on Base, and with how much.
func GraduatedTopic() common.Hash {
	return common.Keccak256([]byte("Graduated(uint256,address,address,uint256,uint256)"))
}

// GradWlxsMintNonce namespaces a graduation's wLXS mint into a range disjoint from the
// peg's sequential lock nonces. The graduation pool is seeded with the SAME wLXS the peg
// mints, so both call wLXS.mint against ONE shared mintedNonce mapping. Peg locks number
// 0,1,2,... (well under 2^64); shifting the graduation nonce up by 2^192 puts graduation
// mints in a range a peg lock can never reach, so the two can never collide on a nonce.
func GradWlxsMintNonce(gradNonce *big.Int) *big.Int {
	offset := new(big.Int).Lsh(big.NewInt(1), 192)
	return new(big.Int).Add(offset, gradNonce)
}

// --- WrappedToken calldata (runs on Base) ---

// WrappedTokenMintCalldata builds mint(nonce, to, amount). nonce is the Graduated
// event's nonce; a repeat is rejected, so double-minting a graduation is impossible.
func WrappedTokenMintCalldata(nonce *big.Int, to common.Address, amount *big.Int) []byte {
	d := append([]byte{0x83, 0x6a, 0x10, 0x40}, uint256Word(nonce)...) // mint(uint256,address,uint256)
	d = append(d, addrWord(to)...)
	return append(d, uint256Word(amount)...)
}
func WrappedTokenRedeemCalldata(amount *big.Int) []byte {
	return append([]byte{0xdb, 0x00, 0x6a, 0x75}, uint256Word(amount)...) // redeem(uint256)
}
func WrappedTokenTransferCalldata(to common.Address, amount *big.Int) []byte {
	d := append([]byte{0xa9, 0x05, 0x9c, 0xbb}, addrWord(to)...) // transfer(address,uint256)
	return append(d, uint256Word(amount)...)
}
func WrappedTokenBalanceCalldata(who common.Address) []byte {
	return append([]byte{0x70, 0xa0, 0x82, 0x31}, addrWord(who)...) // balanceOf(address)
}

// WrappedRedeemTopic is topic0 of Redeem(uint256,address,uint256): the operator watches
// it on Base to release the locked coin on LXS.
func WrappedRedeemTopic() common.Hash {
	return common.Keccak256([]byte("Redeem(uint256,address,uint256)"))
}
