package core

import (
	"bytes"
	"math/big"

	"lxs/types"
)

// Tip is a chain end weighed for comparison: the head block plus the total
// difficulty of the chain leading to it. Fork choice needs the second number
// because height is free but accumulated work is not.
type Tip struct {
	Header          *types.Header
	TotalDifficulty *big.Int
}

// ForkChoice decides which of two competing chain tips wins. The implementation
// plugs in the consensus rule (HeaviestChain for PoW); nothing else knows which is
// running. The contract, breaking any of which silently partitions the network:
//
//  1. Total. For any two distinct tips it must pick one; a tie is not allowed.
//  2. Deterministic. No clocks, randomness, or arrival order.
//  3. Pure. Depends only on the tips, never on which one is currently held.
type ForkChoice interface {
	// Better reports whether candidate should replace current as head.
	Better(candidate, current *Tip) bool
	Name() string
}

// HeaviestChain prefers the chain with the most accumulated work, breaking ties by
// the smaller block hash. Work is not free, so the chain cannot be taken by minting
// empty blocks. The tie-break ensures two equal-work tips seen in opposite orders
// resolve to the same head.
type HeaviestChain struct{}

func (HeaviestChain) Name() string { return "heaviest-chain (total difficulty)" }

func (HeaviestChain) Better(candidate, current *Tip) bool {
	if c := candidate.TotalDifficulty.Cmp(current.TotalDifficulty); c != 0 {
		return c > 0
	}
	// Equal work: pick by hash. Arbitrary, but identical on every node.
	ch, cur := candidate.Header.Hash(), current.Header.Hash()
	return bytes.Compare(ch[:], cur[:]) < 0
}
