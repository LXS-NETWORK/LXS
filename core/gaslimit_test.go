package core

import (
	"errors"
	"testing"

	"lxs/state"
	"lxs/types"
)

// ApplyBlock rejects a block whose Header.GasLimit jumps beyond the ±parent/1024
// bound or drops below the floor, so a miner cannot declare a huge limit and force
// the network to re-execute an oversized block. An honest block (limit copied from
// the parent) clears the gate.
func TestBlockGasLimitBounded(t *testing.T) {
	dev := newKey(t)
	bc := NewMemBlockchain(testGenesis(dev.Address()))
	parent := bc.Head()
	parentState := bc.StateSnapshot()

	br := newBranch(t, bc, parent)
	blk := br.next() // valid empty block; GasLimit copied from the parent (zero delta)

	// withLimit returns a shallow copy of blk with a tampered gas limit. Only the
	// gas-limit gate (before execution) is exercised, so the state root is irrelevant.
	withLimit := func(limit uint64) *types.Block {
		hdr := *blk.Header
		hdr.GasLimit = limit
		return &types.Block{Header: &hdr, Txs: blk.Txs}
	}

	// The honest block clears the gas-limit gate.
	if _, _, err := state.ApplyBlock(parentState, blk, parent.Header); errors.Is(err, state.ErrBadGasLimit) {
		t.Fatal("honest parent-copied gas limit was rejected by the bound")
	}
	// 2x the parent limit, beyond ±1/1024, rejected before any execution.
	if _, _, err := state.ApplyBlock(parentState, withLimit(parent.Header.GasLimit*2), parent.Header); !errors.Is(err, state.ErrBadGasLimit) {
		t.Fatalf("out-of-bounds (2x) gas limit accepted: %v", err)
	}
	// below the floor, rejected.
	if _, _, err := state.ApplyBlock(parentState, withLimit(state.MinBlockGasLimit-1), parent.Header); !errors.Is(err, state.ErrBadGasLimit) {
		t.Fatalf("below-floor gas limit accepted: %v", err)
	}
}

// A non-genesis block (Height>0) applied with a nil parent header must fail with
// ErrBadParent, never silently skip every parent check.
func TestApplyBlockRejectsNilParentForNonGenesis(t *testing.T) {
	dev := newKey(t)
	bc := NewMemBlockchain(testGenesis(dev.Address()))
	parentState := bc.StateSnapshot()
	br := newBranch(t, bc, bc.Head())
	blk := br.next() // height 1

	if _, _, err := state.ApplyBlock(parentState, blk, nil); !errors.Is(err, state.ErrBadParent) {
		t.Fatalf("nil parent on a height-%d block accepted: %v, want ErrBadParent", blk.Height(), err)
	}
}
