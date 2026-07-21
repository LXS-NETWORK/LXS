package contracts

import (
	"math/big"

	"lxs/common"
)

// Uniswap V2 Router integration for graduation. We do NOT deploy a DEX: on Base the
// canonical UniswapV2Router02 is already live at
// 0x4752ba5dbc23f44d87826276bf6fd6b1c372ad24, and its addLiquidity creates the pair if
// it does not exist and seeds it in one call. The operator's graduation orchestrator
// approves the router for the two wrapped tokens, then calls this. Coinbase's DEX
// indexer (0x/1inch) then picks the pool up.

// UniV2Router02Base is the canonical UniswapV2Router02 on Base mainnet.
const UniV2Router02Base = "0x4752ba5dbc23f44d87826276bf6fd6b1c372ad24"

// UniV2AddLiquidityCalldata builds addLiquidity(tokenA, tokenB, amtADesired, amtBDesired,
// amtAMin, amtBMin, to, deadline). The mins encode the caller's price belief: if the pair
// already holds a different ratio the call reverts rather than seed at a bad price. `to`
// receives the LP tokens; `deadline` is a unix timestamp past which the tx is rejected.
func UniV2AddLiquidityCalldata(tokenA, tokenB common.Address, amtADesired, amtBDesired, amtAMin, amtBMin *big.Int, to common.Address, deadline *big.Int) []byte {
	d := []byte{0xe8, 0xe3, 0x37, 0x00} // addLiquidity(address,address,uint256,uint256,uint256,uint256,address,uint256)
	d = append(d, addrWord(tokenA)...)
	d = append(d, addrWord(tokenB)...)
	d = append(d, uint256Word(amtADesired)...)
	d = append(d, uint256Word(amtBDesired)...)
	d = append(d, uint256Word(amtAMin)...)
	d = append(d, uint256Word(amtBMin)...)
	d = append(d, addrWord(to)...)
	return append(d, uint256Word(deadline)...)
}
