package contracts

import (
	_ "embed"
)

// MockRouterV2 is a UniswapV2Router02.addLiquidity stand-in used only by tests, so the
// graduation orchestrator's approve + addLiquidity path can run end to end on the local
// VM without a real DEX. In production the canonical router on Base (UniV2Router02Base)
// is used instead — this bytecode is never deployed to a live network.
//
//go:embed solidity/MockRouterV2.bin
var mockRouterV2Hex string

// MockRouterV2Init is the deploy bytecode for the test-only mock router (no constructor args).
func MockRouterV2Init() []byte { return decodeHex("MockRouterV2", mockRouterV2Hex) }
