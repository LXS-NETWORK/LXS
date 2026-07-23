package mempool

import (
	"path/filepath"
	"testing"

	"lxs/types"
)

// A local tx must survive a "restart": AddLocal to one pool, then a fresh pool
// pointed at the same journal replays it. Gossiped (plain Add) txs must NOT be
// journaled — they live on their origin node.
func TestJournalSurvivesRestart(t *testing.T) {
	k := mustKey(t)
	path := filepath.Join(t.TempDir(), "txjournal.jsonl")

	p1 := New(64)
	p1.EnableJournal(path, testChainID) // no file yet
	local := txFrom(t, k, 0, 100, 1)
	if err := p1.AddLocal(local, testChainID); err != nil {
		t.Fatalf("AddLocal: %v", err)
	}
	gossiped := txFrom(t, k, 1, 100, 1)
	if err := p1.Add(gossiped, testChainID); err != nil { // plain Add = gossiped, not journaled
		t.Fatalf("Add: %v", err)
	}

	// "Restart": a brand-new pool replays the journal.
	p2 := New(64)
	loaded := p2.EnableJournal(path, testChainID)
	if loaded != 1 {
		t.Fatalf("replayed %d txs, want 1 (only the local one)", loaded)
	}
	if _, ok := p2.Get(local.Hash()); !ok {
		t.Fatal("the local tx was not restored from the journal")
	}
	if _, ok := p2.Get(gossiped.Hash()); ok {
		t.Fatal("a gossiped tx was journaled — it must not be")
	}
}

// Once a journaled tx is mined (Remove), it must be compacted out so a later
// restart does not resurrect an already-spent nonce.
func TestJournalCompactsMinedTx(t *testing.T) {
	k := mustKey(t)
	path := filepath.Join(t.TempDir(), "txjournal.jsonl")

	p1 := New(64)
	p1.EnableJournal(path, testChainID)
	tx := txFrom(t, k, 0, 100, 1)
	if err := p1.AddLocal(tx, testChainID); err != nil {
		t.Fatalf("AddLocal: %v", err)
	}
	p1.Remove([]*types.Transaction{tx}) // mined

	p2 := New(64)
	if loaded := p2.EnableJournal(path, testChainID); loaded != 0 {
		t.Fatalf("replayed %d txs after the tx was mined, want 0", loaded)
	}
}
