// gradhex prints the deploy bytecode / calldata hex the graduation end-to-end script
// needs, reusing the same contracts helpers the node and daemon use. It is a test/dev
// aid — a thin shell over contracts.* so a bash harness can drive real `lxs send` txs
// without hand-encoding ABI. Not part of the product.
package main

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"os"

	"lxs/common"
	"lxs/contracts"
)

func main() {
	if len(os.Args) < 2 {
		fatal("usage: gradhex <op> [args]")
	}
	out := func(b []byte) { fmt.Println("0x" + hex.EncodeToString(b)) }
	switch os.Args[1] {
	case "vault-init": // <operator> <minLiqWei>
		out(contracts.GraduationVaultInit(addr(2), bigArg(3)))
	case "wlxs-init": // <operator>
		out(contracts.WrappedLXSInit(addr(2)))
	case "mockrouter-init":
		out(contracts.MockRouterV2Init())
	case "factory-init": // <feeRecipient> <feeBps> <swapFactory> <wlxs>
		out(contracts.PumpFactoryInit(addr(2), bigArg(3).Uint64(), addr(4), addr(5)))
	case "swap-wlxs-init": // (no args) — WETH-style wrapped LXS for the native DEX
		out(contracts.WlxsInit())
	case "swap-factory-init": // <feeToSetter> — the LxsSwap (Uniswap-V2) factory
		out(contracts.LxsSwapFactoryInit(addr(2)))
	case "swap-router-init": // <factory> <wlxs>
		out(contracts.LxsSwapRouterInit(addr(2), addr(3)))
	case "usertoken": // <name> <symbol> <supplyWei>
		out(contracts.UserTokenDeploy(os.Args[2], os.Args[3], bigArg(4)))
	case "approve": // <spender> <amountWei>
		out(contracts.ApproveCalldata(addr(2), bigArg(3)))
	case "graduate": // <coin> <tokenAmountWei>
		out(contracts.GraduateCalldata(addr(2), bigArg(3)))
	case "balanceof": // <holder>
		out(contracts.BalanceOfCalldata(addr(2)))
	default:
		fatal("unknown op %q", os.Args[1])
	}
}

func addr(i int) common.Address {
	a, err := common.AddressFromHex(os.Args[i])
	if err != nil {
		fatal("bad address %q: %v", os.Args[i], err)
	}
	return a
}

func bigArg(i int) *big.Int {
	n, ok := new(big.Int).SetString(os.Args[i], 10)
	if !ok {
		fatal("bad number %q", os.Args[i])
	}
	return n
}

func fatal(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "gradhex: "+f+"\n", a...)
	os.Exit(1)
}
