package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/state"
	"lxs/types"
)

// Producer and validators must agree on what NUMBER returns, or they compute
// different state roots from the same block and the chain splits. A contract that
// SSTOREs block.number is mined then re-validated by InsertBlock; a disagreement
// would fail the recomputed root. Acceptance, plus the stored value equalling the
// block height, proves agreement.
func TestBlockContextIsConsensusConsistent(t *testing.T) {
	dev := newKey(t)
	bc := NewMemBlockchain(testGenesis(dev.Address()))
	br := newBranch(t, bc, bc.Head())
	br.proposer = common.Address{0x10}

	// Runtime: NUMBER ; PUSH1 0 ; SSTORE ; STOP — store block.number in slot 0.
	runtime := []byte{0x43, 0x60, 0x00, 0x55, 0x00}
	// Init: PUSH5 <runtime> ; PUSH1 0 ; MSTORE ; PUSH1 5 ; PUSH1 27 ; RETURN.
	initCode := append([]byte{0x64}, runtime...)
	initCode = append(initCode, 0x60, 0x00, 0x52, 0x60, 0x05, 0x60, 0x1b, 0xf3)

	deploy := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: testChainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 1_000_000, GasPrice: big.NewInt(1), Data: initCode,
	}
	if err := deploy.Sign(dev); err != nil {
		t.Fatal(err)
	}
	blk1 := br.next(deploy)
	if err := bc.InsertBlock(blk1); err != nil {
		t.Fatalf("deploy block rejected (block-context divergence?): %v", err)
	}
	contract := state.CreateAddress(dev.Address(), 0)

	// Call it: the contract stores whatever NUMBER returns during this block.
	call := types.NewTransaction(testChainID, 1, contract, big.NewInt(0), 1_000_000, big.NewInt(1), nil)
	if err := call.Sign(dev); err != nil {
		t.Fatal(err)
	}
	blk2 := br.next(call)
	if err := bc.InsertBlock(blk2); err != nil {
		t.Fatalf("call block rejected — producer and validator disagree on the block context: %v", err)
	}

	// The stored NUMBER must equal the height of the block the call ran in.
	stored := new(big.Int).SetBytes(bc.StateSnapshot().GetStorage(contract, common.Hash{}).Bytes())
	if stored.Uint64() != blk2.Height() {
		t.Fatalf("contract stored NUMBER = %d, want the block height %d", stored, blk2.Height())
	}
}
