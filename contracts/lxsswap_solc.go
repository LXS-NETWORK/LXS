package contracts

import (
	_ "embed"
	"math/big"

	"lxs/common"
)

// LxsSwap — a Uniswap-V2-compatible AMM native to LXS, compiled from
// solidity/LxsSwap.sol (solc 0.8.26, evmVersion=istanbul) and embedded so tests
// need no toolchain. This is the on-chain "DEX-market" a price aggregator reads:
// it discovers pairs from LxsSwapFactory's PairCreated event and reads each pair's
// canonical V2 views/events. Launchpad coins graduate into a COIN/WLXS pair here.
//
// Not part of the immutable chain — ordinary contracts the operator deploys once.

//go:embed solidity/WLXS.bin
var wlxsHex string

//go:embed solidity/LxsSwapFactory.bin
var lxsSwapFactoryHex string

// WlxsInit is the deploy bytecode for WLXS (WETH-style wrapper for native LXS; no
// constructor args).
func WlxsInit() []byte { return decodeHex("WLXS", wlxsHex) }

// LxsSwapFactoryInit builds the deploy bytecode for LxsSwapFactory(feeToSetter).
func LxsSwapFactoryInit(feeToSetter common.Address) []byte {
	return append(decodeHex("LxsSwapFactory", lxsSwapFactoryHex), addrWord(feeToSetter)...)
}

// --- WLXS calldata (WETH-style wrapper) ---
func WlxsDepositCalldata() []byte { return []byte{0xd0, 0xe3, 0x0d, 0xb0} } // deposit()
func WlxsWithdrawCalldata(amount *big.Int) []byte {
	return append([]byte{0x2e, 0x1a, 0x7d, 0x4d}, uint256Word(amount)...) // withdraw(uint256)
}

// --- LxsSwapFactory calldata ---
// CreatePair(tokenA, tokenB) — deploys the pair (idempotent: reverts if it exists).
func SwapCreatePairCalldata(a, b common.Address) []byte {
	d := append([]byte{0xc9, 0xc6, 0x53, 0x96}, addrWord(a)...) // createPair(address,address)
	return append(d, addrWord(b)...)
}

// GetPair(tokenA, tokenB) — reads the pair address (0 if none).
func SwapGetPairCalldata(a, b common.Address) []byte {
	d := append([]byte{0xe6, 0xa4, 0x39, 0x05}, addrWord(a)...) // getPair(address,address)
	return append(d, addrWord(b)...)
}

func SwapAllPairsLengthCalldata() []byte { return []byte{0x57, 0x4f, 0x2b, 0xa3} } // allPairsLength()

// --- LxsSwapPair calldata ---
// The router/graduation transfers both tokens in, then calls Mint(to) to receive LP.
func SwapPairMintCalldata(to common.Address) []byte {
	return append([]byte{0x6a, 0x62, 0x78, 0x42}, addrWord(to)...) // mint(address)
}
func SwapPairBurnCalldata(to common.Address) []byte {
	return append([]byte{0x89, 0xaf, 0xcb, 0x44}, addrWord(to)...) // burn(address)
}

// Swap(amount0Out, amount1Out, to, data) — canonical V2 swap (022c0d9f). data is
// empty for an ordinary swap; the input token must already have been transferred in.
func SwapPairSwapCalldata(amount0Out, amount1Out *big.Int, to common.Address) []byte {
	d := append([]byte{0x02, 0x2c, 0x0d, 0x9f}, uint256Word(amount0Out)...) // swap(uint256,uint256,address,bytes)
	d = append(d, uint256Word(amount1Out)...)
	d = append(d, addrWord(to)...)
	d = append(d, uint256Word(big.NewInt(0x80))...) // bytes offset
	return append(d, uint256Word(big.NewInt(0))...) // bytes length 0
}

func SwapPairGetReservesCalldata() []byte { return []byte{0x09, 0x02, 0xf1, 0xac} } // getReserves()
func SwapPairToken0Calldata() []byte      { return []byte{0x0d, 0xfe, 0x16, 0x81} } // token0()
func SwapPairToken1Calldata() []byte      { return []byte{0xd2, 0x12, 0x20, 0xa7} } // token1()

// SwapPairCreatedTopic is topic0 of PairCreated(address,address,address,uint256):
// an aggregator filters on this to enumerate every market.
func SwapPairCreatedTopic() common.Hash {
	return common.Keccak256([]byte("PairCreated(address,address,address,uint256)"))
}

// SwapSyncTopic / SwapSwapTopic are the canonical V2 event topics an indexer reads
// to track a pool's reserves and trades.
func SwapSyncTopic() common.Hash { return common.Keccak256([]byte("Sync(uint112,uint112)")) }
func SwapSwapTopic() common.Hash {
	return common.Keccak256([]byte("Swap(address,uint256,uint256,uint256,uint256,address)"))
}

// --- LxsSwapRouter: the periphery that makes trading a pool safe from a wallet ---

//go:embed solidity/LxsSwapRouter.bin
var lxsSwapRouterHex string

// LxsSwapRouterInit builds the deploy bytecode for LxsSwapRouter(factory, wlxs).
func LxsSwapRouterInit(factory, wlxs common.Address) []byte {
	d := append(decodeHex("LxsSwapRouter", lxsSwapRouterHex), addrWord(factory)...)
	return append(d, addrWord(wlxs)...)
}

// BUY: swapExactLXSForTokens(token, amountOutMin, to, deadline), sent with native value.
func RouterBuyCalldata(token common.Address, amountOutMin *big.Int, to common.Address, deadline *big.Int) []byte {
	d := append([]byte{0xe4, 0x84, 0x8a, 0x9d}, addrWord(token)...)
	d = append(d, uint256Word(amountOutMin)...)
	d = append(d, addrWord(to)...)
	return append(d, uint256Word(deadline)...)
}

// SELL: swapExactTokensForLXS(token, amountIn, amountOutMin, to, deadline). Approve first.
func RouterSellCalldata(token common.Address, amountIn, amountOutMin *big.Int, to common.Address, deadline *big.Int) []byte {
	d := append([]byte{0xe6, 0x4e, 0x82, 0x2c}, addrWord(token)...)
	d = append(d, uint256Word(amountIn)...)
	d = append(d, uint256Word(amountOutMin)...)
	d = append(d, addrWord(to)...)
	return append(d, uint256Word(deadline)...)
}

// quoteBuy(token, lxsIn) / quoteSell(token, tokenIn) — read-only UI quotes.
func RouterQuoteBuyCalldata(token common.Address, lxsIn *big.Int) []byte {
	return append(append([]byte{0x0d, 0x7a, 0x94, 0xf6}, addrWord(token)...), uint256Word(lxsIn)...)
}
func RouterQuoteSellCalldata(token common.Address, tokenIn *big.Int) []byte {
	return append(append([]byte{0xd9, 0x8b, 0x2f, 0x5c}, addrWord(token)...), uint256Word(tokenIn)...)
}
