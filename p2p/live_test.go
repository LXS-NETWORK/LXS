package p2p

import (
	"testing"

	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/store"
	"lxs/types"
)

// End to end through the real Producer, not the test forger: produce -> insert
// -> announce hook -> gossip -> three followers, over a hostile switch. The
// forger tests prove the gossip logic; this proves the wiring, that the hook
// fires, after insertion, once.
func TestProducerHookDrivesTheNetwork(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Duplicates: 2, Shuffle: true, Seed: 42})

	bc, err := core.NewBlockchain(store.NewMemory(), gen, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pool := mempool.New(1024)
	core.BindMempool(bc, pool)
	coinbase, _ := crypto.GenerateKey()
	prod := core.NewProducer(bc, pool, coinbase.Address())

	n0 := sw.Join("node0")
	g0, err := NewGossip(n0, bc)
	if err != nil {
		t.Fatal(err)
	}

	announced := 0
	prod.SetOnBlock(func(b *types.Block) {
		announced++
		if err := g0.Announce(b); err != nil {
			t.Errorf("announce failed: %v", err)
		}
	})

	followers := []*testNode{
		newTestNode(t, sw, "node1", gen),
		newTestNode(t, sw, "node2", gen),
		newTestNode(t, sw, "node3", gen),
	}

	for i := 0; i < 10; i++ {
		if _, err := prod.Seal(); err != nil {
			t.Fatalf("producing block %d: %v", i, err)
		}
	}

	if announced != 10 {
		t.Fatalf("announce hook fired %d times, want 10", announced)
	}
	want := bc.Head().Hash()
	if bc.Head().Height() != 10 {
		t.Fatalf("producer height: got %d want 10", bc.Head().Height())
	}

	for _, f := range followers {
		if f.bc.Head().Hash() != want {
			t.Fatalf("%s head %s != producer head %s",
				f.id, f.bc.Head().Hash().Hex(), want.Hex())
		}
		if f.bc.StateSnapshot().Root() != f.bc.Head().Header.StateRoot {
			t.Fatalf("%s agrees on history but not on the money", f.id)
		}
	}
}
