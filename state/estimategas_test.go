package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/types"
)

// A contract-creation estimate must run the init code and include the per-byte
// code-storage cost, not return the flat intrinsic 21000. Otherwise MetaMask /
// ethers deploy with a 21000 limit and the tx fails out-of-gas.
func TestEstimateGasForContractCreation(t *testing.T) {
	runtime := []byte{0x60, 0x00, 0x35, 0x60, 0x00, 0x55, 0x00} // 7 bytes
	initCode := []byte{0x66}                                    // PUSH7 <runtime>
	initCode = append(initCode, runtime...)
	initCode = append(initCode, 0x60, 0x00, 0x52, 0x60, 0x07, 0x60, 0x19, 0xf3) // MSTORE; RETURN 7 from 25

	s := New()
	gas, err := EstimateGas(s, common.Address{0x01}, nil, initCode, big.NewInt(0), 30_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if gas <= types.IntrinsicGas {
		t.Fatalf("create estimate = %d, must exceed intrinsic %d (a deploy was mis-estimated at 21000)", gas, types.IntrinsicGas)
	}
	if want := types.IntrinsicGas + codeDepositGas*uint64(len(runtime)); gas < want {
		t.Fatalf("create estimate %d omits code-deposit gas (want >= %d)", gas, want)
	}
}

// The estimate must be the MINIMUM EXECUTABLE limit, not a number that merely runs
// under a fat gasCap. This is the EIP-150 63/64 trap and the exact reason a single
// run at gasCap under-estimates: a CALL forwards all-but-1/64 of the caller's
// remaining gas, so a sub-call that just barely completes when the whole 30M cap is
// available can be starved by that reserved 1/64 once the tx is resubmitted with
// the tighter measured limit — and if the caller reverts on a failed sub-call, the
// "estimated" tx reverts on-chain.
//
// callee expands memory to ~512 KiB (a large, limit-independent quadratic gas
// cost). caller CALLs it forwarding GAS (so the 63/64 cap bites) and REVERTs if the
// call fails. A single run at gasCap measures g = intrinsic + caller-overhead +
// callee-cost; resubmitted at g, the CALL forwards only 63/64·(≈callee-cost), the
// callee is short by ~1/64 and out-of-gases, the caller reverts. The binary search
// instead returns g' large enough that 63/64 of the forwarded slice still covers
// the callee — proven here by: a call at g' SUCCEEDS and at g'-1 FAILS.
func TestEstimateGasNestedCallIsExecutable(t *testing.T) {
	caller := key(t)
	s := New()
	s.Credit(caller.Address(), common.LXS(100))

	// callee: PUSH1 0; PUSH3 0x080000; MSTORE; STOP — touches offset 512 KiB, forcing
	// a large memory-expansion charge (all up-front, so it out-of-gases cleanly if
	// underfunded).
	callee := common.Address{0xCA}
	s.SetCode(callee, []byte{0x60, 0x00, 0x62, 0x08, 0x00, 0x00, 0x52, 0x00})

	// caller runtime: CALL(gas=GAS, addr=callee, value=0, in=0/0, out=0/0); if the
	// call failed (returns 0), REVERT; else STOP. Offsets are fixed below; dest is
	// the JUMPDEST byte index.
	target := common.Address{0xAA}
	code := []byte{
		0x60, 0x00, // PUSH1 0   retLen
		0x60, 0x00, // PUSH1 0   retOff
		0x60, 0x00, // PUSH1 0   argsLen
		0x60, 0x00, // PUSH1 0   argsOff
		0x60, 0x00, // PUSH1 0   value
		0x73, // PUSH20 callee
	}
	code = append(code, callee[:]...)
	code = append(code,
		0x5a,       // GAS        (forward all — 63/64 cap applies)
		0xf1,       // CALL       -> success (1/0)
		0x15,       // ISZERO
		0x60, 0x26, // PUSH1 38   (dest of JUMPDEST)
		0x57,       // JUMPI
		0x00,       // STOP            (success path)
		0x5b,       // JUMPDEST  <- index 38
		0x60, 0x00, // PUSH1 0
		0x60, 0x00, // PUSH1 0
		0xfd, // REVERT          (failure path)
	)
	if code[38] != 0x5b {
		t.Fatalf("JUMPDEST not at index 38 (got 0x%02x) — reassemble", code[38])
	}
	s.SetCode(target, code)

	g, err := EstimateGas(s.Copy(), caller.Address(), &target, nil, big.NewInt(0), 30_000_000)
	if err != nil {
		t.Fatalf("estimate errored: %v", err)
	}

	apply := func(st *State, gas uint64) uint64 {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: &target,
			Value: big.NewInt(0), GasLimit: gas, GasPrice: big.NewInt(1), Data: nil}
		if err := tx.Sign(caller); err != nil {
			t.Fatal(err)
		}
		_, status, _, err := ApplyTx(st, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return status
	}

	if st := apply(s.Copy(), g); st != types.ReceiptSuccess {
		t.Fatalf("call at estimated gas %d REVERTED — estimate is not executable (63/64 under-estimate)", g)
	}
	if st := apply(s.Copy(), g-1); st == types.ReceiptSuccess {
		t.Fatalf("call at estimate-1 (%d) still succeeded — estimate %d is not the minimal executable limit", g-1, g)
	}
}

// A value-forwarding call must be estimated against the SAME value credit ApplyTx
// performs. The estimator exists for "buy/sell through a router" — value > 0. If
// probe() runs the code without crediting the callee its value, the router's inner
// CALL that forwards msg.value hits opCall's balance gate (vm/call.go), fail-fasts,
// and the router reverts — so EstimateGas fabricates a revert that the real,
// value-credited tx never has. This forwards CALLVALUE to a sink and reverts on
// failure, so a missing credit makes EstimateGas error while ApplyTx succeeds.
func TestEstimateGasCreditsForwardedValue(t *testing.T) {
	caller := key(t)
	s := New()
	V := common.LXS(3)
	// Fund the sender to cover value + gas for the real tx.
	s.Credit(caller.Address(), common.LXS(100))

	sink := common.Address{0x5B}
	s.SetCode(sink, []byte{0x00}) // STOP: accepts value, does nothing

	// router: CALL(gas=GAS, addr=sink, value=CALLVALUE, in=0/0, out=0/0); REVERT if
	// the call failed. JUMPDEST lands at index 37.
	router := common.Address{0x4D}
	code := []byte{
		0x60, 0x00, // PUSH1 0   retLen
		0x60, 0x00, // PUSH1 0   retOff
		0x60, 0x00, // PUSH1 0   argsLen
		0x60, 0x00, // PUSH1 0   argsOff
		0x34, // CALLVALUE (forward msg.value)
		0x73, // PUSH20 sink
	}
	code = append(code, sink[:]...)
	code = append(code,
		0x5a,       // GAS
		0xf1,       // CALL
		0x15,       // ISZERO
		0x60, 0x25, // PUSH1 37
		0x57,       // JUMPI
		0x00,       // STOP
		0x5b,       // JUMPDEST <- 37
		0x60, 0x00, // PUSH1 0
		0x60, 0x00, // PUSH1 0
		0xfd, // REVERT
	)
	if code[37] != 0x5b {
		t.Fatalf("JUMPDEST not at 37 (got 0x%02x)", code[37])
	}
	s.SetCode(router, code)

	g, err := EstimateGas(s.Copy(), caller.Address(), &router, nil, V, 30_000_000)
	if err != nil {
		t.Fatalf("estimate of a value-forwarding call errored: %v (probe did not credit the forwarded value)", err)
	}

	// And the estimate is executable: the real tx at g succeeds.
	tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: &router,
		Value: V, GasLimit: g, GasPrice: big.NewInt(1), Data: nil}
	if err := tx.Sign(caller); err != nil {
		t.Fatal(err)
	}
	_, st, _, err := ApplyTx(s.Copy(), tx, common.Address{}, 30_000_000)
	if err != nil {
		t.Fatalf("apply errored: %v", err)
	}
	if st != types.ReceiptSuccess {
		t.Fatalf("value-forwarding tx at estimated gas %d reverted on-chain — estimate not executable", g)
	}
}

// A reverting execution must be surfaced as an error, not a silent under-estimate
// that gets mined as a failed tx — so a wallet can warn the user first.
func TestEstimateGasSurfacesRevert(t *testing.T) {
	revertInit := []byte{0x60, 0x00, 0x60, 0x00, 0xfd} // PUSH1 0; PUSH1 0; REVERT
	s := New()
	if _, err := EstimateGas(s, common.Address{0x01}, nil, revertInit, big.NewInt(0), 30_000_000); err == nil {
		t.Fatal("estimateGas must surface a revert as an error, not a silent under-estimate")
	}
}
