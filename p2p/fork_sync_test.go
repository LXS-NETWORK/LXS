package p2p

import (
	"testing"

	"lxs/crypto"
)

// A node on a lighter fork that diverged below its head must sync onto a peer's
// heavier fork: the case a multi-miner network produces. The syncer finds the
// common ancestor and reorgs, without penalising the peer (a fork is not a lie).
func TestForkSyncReorgsOntoHeavierChain(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 30})

	// Server: a HEAVIER chain (8 blocks). Client: a lighter one (3), diverging at
	// height 1 because each forger mines with its own proposer.
	serverBC := newBC(t, gen)
	buildChain(t, serverBC, 8)
	if _, err := NewSyncer(sw.Join("server"), serverBC); err != nil {
		t.Fatal(err)
	}

	clientBC := newBC(t, gen)
	scorer := NewScorer(3)
	client, err := NewSyncer(sw.Join("client"), clientBC, WithSyncScorer(scorer))
	if err != nil {
		t.Fatal(err)
	}
	buildChain(t, clientBC, 3)

	// sanity: the two heads really do differ (a genuine fork, not a prefix).
	if clientBC.Head().Hash() == serverBC.Head().Hash() {
		t.Fatal("test setup: chains did not diverge")
	}

	client.SyncFrom("server")

	if clientBC.Head().Hash() != serverBC.Head().Hash() {
		t.Fatalf("client did not reorg onto the heavier chain: client %d %s, server %d %s",
			clientBC.Head().Height(), clientBC.Head().Hash().Hex(),
			serverBC.Head().Height(), serverBC.Head().Hash().Hex())
	}
	if scorer.Penalty("server") != 0 {
		t.Fatalf("the peer was penalised for being on a fork (penalty %d) — a fork is not a lie", scorer.Penalty("server"))
	}
}

// Syncing from a peer on a lighter fork pulls its blocks as a side branch, does
// not reorg, leaves our head unchanged, and terminates instead of re-pulling the
// same known fork forever. This exercises the progress guard.
func TestForkSyncDoesNotReorgOntoLighterChainAndTerminates(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 31})

	serverBC := newBC(t, gen)
	buildChain(t, serverBC, 3) // lighter
	if _, err := NewSyncer(sw.Join("server"), serverBC); err != nil {
		t.Fatal(err)
	}

	clientBC := newBC(t, gen)
	scorer := NewScorer(3)
	client, err := NewSyncer(sw.Join("client"), clientBC, WithSyncScorer(scorer))
	if err != nil {
		t.Fatal(err)
	}
	buildChain(t, clientBC, 8) // heavier
	headBefore := clientBC.Head().Hash()

	// If this returns, termination works: a missing progress guard would spin
	// forever re-pulling the peer's known fork and the test would hang.
	client.SyncFrom("server")

	if clientBC.Head().Hash() != headBefore {
		t.Fatal("client abandoned its heavier chain for a lighter fork")
	}
	if scorer.Penalty("server") != 0 {
		t.Fatalf("penalised a peer for a lighter fork (penalty %d)", scorer.Penalty("server"))
	}
}
