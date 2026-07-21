package p2p

import (
	"testing"
	"time"

	"lxs/core"
	"lxs/crypto"
	"lxs/store"
)

// A block dated ahead of a node's clock is deferred, not rejected, and the peer
// that relayed it is not penalised. Banning honest relayers over clock skew
// would fragment the mesh, so the future-block path stays off the scoring path.
func TestGossipDefersFutureBlockWithoutPenalty(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 7})

	attacker := newTestNode(t, sw, "attacker", gen)

	// Forge a valid block on genesis (difficulty floor, nonce 0 is a proof).
	blk := newForger(t, attacker.bc, attacker.bc.Head()).next()

	// The victim's clock sits before the block's timestamp by more than the drift
	// ceiling, so from its view the block is in the future.
	victimNow := time.UnixMilli(blk.Header.Timestamp - core.MaxFutureDriftMs - 5_000)
	scorer := NewScorer(3)
	vbc, err := core.NewBlockchain(store.NewMemory(), gen, core.Options{Now: func() time.Time { return victimNow }})
	if err != nil {
		t.Fatal(err)
	}
	vn := sw.Join("victim")
	vg, err := NewGossip(vn, vbc, WithScorer(scorer))
	if err != nil {
		t.Fatal(err)
	}

	// The attacker's clock is real-now and the block is dated in the past
	// relative to it, so it accepts and announces normally.
	if err := attacker.bc.InsertBlock(blk); err != nil {
		t.Fatalf("attacker insert: %v", err)
	}
	attacker.g.Announce(blk)

	s := vg.Snapshot()
	if s.Deferred != 1 {
		t.Fatalf("future block: Deferred = %d, want 1", s.Deferred)
	}
	if s.Rejected != 0 {
		t.Fatalf("a future block must NOT count as Rejected (got %d) — that path bans the peer", s.Rejected)
	}
	if scorer.Penalty("attacker") != 0 || scorer.Banned("attacker") {
		t.Fatalf("an honest peer relaying a future block must not be penalised (penalty %d)", scorer.Penalty("attacker"))
	}
	if vbc.HasBlock(blk.Hash()) {
		t.Fatal("a deferred future block must not be stored")
	}
}
