package core

import (
	"math/big"
	"testing"

	"lxs/common"
)

// The read accessors must return correct head values, and BalanceAt must return a
// COPY — a caller mutating it must not corrupt chain state (the whole point of not
// cloning the world is defeated if the shared balance pointer leaks out).
func TestReadAccessors(t *testing.T) {
	addr := common.Address{0x42}
	bc := NewMemBlockchain(testGenesis(addr))

	if got := bc.BalanceAt(addr); got.Sign() <= 0 {
		t.Fatalf("BalanceAt = %s, want the funded genesis balance", got)
	}
	if got, want := bc.BalanceAt(addr), bc.StateSnapshot().Balance(addr); got.Cmp(want) != 0 {
		t.Fatalf("BalanceAt %s != snapshot %s", got, want)
	}
	if bc.NonceAt(addr) != bc.StateSnapshot().Nonce(addr) {
		t.Fatal("NonceAt disagrees with snapshot")
	}

	// Mutating the returned balance must not touch state.
	b := bc.BalanceAt(addr)
	b.Add(b, big.NewInt(1_000_000))
	if bc.BalanceAt(addr).Cmp(b) == 0 {
		t.Fatal("BalanceAt returned a live pointer — mutation corrupted chain state")
	}
}
