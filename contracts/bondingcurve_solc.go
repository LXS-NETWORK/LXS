package contracts

import (
	_ "embed"
	"math/big"

	"lxs/common"
)

// The bonding-curve launchpad, compiled from solidity/BondingCurve.sol
// (solc/istanbul) and checked in as bytecode. PumpFactory.create(name,symbol)
// spins up a coin instantly tradeable on its own constant-product curve, with no
// upfront liquidity.
//
//go:embed solidity/PumpFactory.bin
var pumpFactoryHex string

// PumpFactoryInit is the deploy bytecode with the constructor args (feeRecipient,
// feeBps) appended. feeRecipient can be an address (platform income) or the burn
// address (deflation). feeBps is capped at 1000 (10%) by the contract.
func PumpFactoryInit(feeRecipient common.Address, feeBps uint64) []byte {
	args := append(addrWord(feeRecipient), uint256Word(new(big.Int).SetUint64(feeBps))...)
	return append(decodeHex("PumpFactory", pumpFactoryHex), args...)
}

const (
	PumpCreateSelector uint32 = 0xdf5c2a2e // create(string,string,bytes,uint256)
	PumpBuySelector    uint32 = 0xd96a094a // buy(uint256)
	PumpSellSelector   uint32 = 0xd79875eb // sell(uint256,uint256)
	PumpQuoteBuy       uint32 = 0x4beb394c // quoteBuy(uint256)
	PumpReserveNative  uint32 = 0xbf36b536 // reserveNative()
	PumpCurveTokens    uint32 = 0x0d93caf7 // curveTokens()
)

// PumpCreatedTopic is topic0 of Created(address,address,string,string): topic1 is
// the creator, data begins with the new coin's address. Read from the receipt (or
// eth_getLogs) to list new coins.
func PumpCreatedTopic() common.Hash {
	return common.Keccak256([]byte("Created(address,address,string,string,bytes)"))
}

// PumpCreateCalldata ABI-encodes create(name, symbol, image, minTokensOut) — two dynamic
// strings, a dynamic bytes thumbnail (may be empty), and a static uint256. When the tx
// carries native value the factory performs the creator's first buy atomically, bounded by
// minTokensOut; pass 0 value / 0 min to create without an initial buy.
func PumpCreateCalldata(name, symbol string, image []byte, minTokensOut *big.Int) []byte {
	encode := func(b []byte) []byte {
		out := uint256Word(big.NewInt(int64(len(b))))
		out = append(out, b...)
		if pad := (32 - len(b)%32) % 32; pad > 0 {
			out = append(out, make([]byte, pad)...)
		}
		return out
	}
	nameEnc := encode([]byte(name))
	symEnc := encode([]byte(symbol))
	imgEnc := encode(image)
	sel := []byte{0xdf, 0x5c, 0x2a, 0x2e}
	// four head words: offsets to name, symbol, image, then the static minTokensOut; head is 0x80.
	head := uint256Word(big.NewInt(0x80))
	head = append(head, uint256Word(big.NewInt(int64(0x80+len(nameEnc))))...)
	head = append(head, uint256Word(big.NewInt(int64(0x80+len(nameEnc)+len(symEnc))))...)
	head = append(head, uint256Word(minTokensOut)...)
	out := append(sel, head...)
	out = append(out, nameEnc...)
	out = append(out, symEnc...)
	return append(out, imgEnc...)
}

// PumpBuyToCalldata ABI-encodes buyTo(to, minTokensOut) — a buy crediting a chosen recipient.
func PumpBuyToCalldata(to common.Address, minTokensOut *big.Int) []byte {
	d := append([]byte{0x09, 0xbd, 0x4c, 0x31}, addrWord(to)...) // buyTo(address,uint256)
	return append(d, uint256Word(minTokensOut)...)
}

// PumpWithdrawFeesCalldata pushes accrued fees to feeRecipient (permissionless, isolated from trading).
func PumpWithdrawFeesCalldata() []byte { return []byte{0x47, 0x63, 0x43, 0xee} } // withdrawFees()

// PumpFeeAccruedCalldata reads the fees waiting to be withdrawn.
func PumpFeeAccruedCalldata() []byte { return []byte{0x2d, 0x52, 0x72, 0xe3} } // feeAccrued()

// PumpBuyCalldata ABI-encodes buy(minTokensOut). The native spent is the tx value.
func PumpBuyCalldata(minTokensOut *big.Int) []byte {
	return append([]byte{0xd9, 0x6a, 0x09, 0x4a}, uint256Word(minTokensOut)...)
}

// PumpSellCalldata ABI-encodes sell(amount, minNativeOut).
func PumpSellCalldata(amount, minNativeOut *big.Int) []byte {
	d := append([]byte{0xd7, 0x98, 0x75, 0xeb}, uint256Word(amount)...)
	return append(d, uint256Word(minNativeOut)...)
}

// PumpReserveNativeCalldata reads the curve's native reserve.
func PumpReserveNativeCalldata() []byte { return []byte{0xbf, 0x36, 0xb5, 0x36} }
