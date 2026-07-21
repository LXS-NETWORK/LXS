package common

import "math/big"

// Network identity.
const (
	NetworkName = "LXS"
	Ticker      = "LXS"

	// DevnetChainID is the only chain id in use; there is no mainnet id yet.
	DevnetChainID = 1337
)

// Denominations. The base unit is the only unit consensus knows; the rest is
// display. The wei/ether split: money never passes through a float, because
// 0.1 + 0.2 != 0.3 in float64.
const (
	Lux      = 1  // base unit — the smallest indivisible amount
	Decimals = 18 // 1 LXS = 10^18 lux
)

// OneLXS is 10^18 lux.
var OneLXS = new(big.Int).Exp(big.NewInt(10), big.NewInt(Decimals), nil)

// LXS converts a whole number of LXS into base units.
// Takes an integer on purpose — there is no float in this path.
func LXS(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), OneLXS)
}
