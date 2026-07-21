package state

import (
	"math/big"
	"testing"

	"lxs/common"
)

// TestJournalRevertRestoresExactly: nested snapshots, repeated writes to one
// account, account creation, empty-account deletion, and storage writes must all
// undo to the exact pre-savepoint state, checked by comparing the state root.
func TestJournalRevertRestoresExactly(t *testing.T) {
	a := common.Address{0xA1}
	b := common.Address{0xB2}
	c := common.Address{0xC3} // will be created inside a snapshot
	contract := common.Address{0xDD}
	slot := common.Hash{0x01}

	s := New()
	s.Credit(a, big.NewInt(1000))
	s.Credit(b, big.NewInt(500))
	s.SetCode(contract, []byte{0x60, 0x00})
	s.SetStorage(contract, slot, common.Hash{0x11})
	baseRoot := s.Root()

	// --- snapshot 0 ---
	s0 := s.Snapshot()
	s.SubBalance(a, big.NewInt(300))                // a: 1000 -> 700
	s.Credit(c, big.NewInt(42))                     // create c
	s.SetStorage(contract, slot, common.Hash{0x22}) // change storage
	s.SubBalance(b, big.NewInt(500))                // b -> 0 (empty => deleted)

	// --- snapshot 1 (nested) ---
	s1 := s.Snapshot()
	s.SubBalance(a, big.NewInt(400))                // a: 700 -> 300 (second write to a)
	s.Credit(c, big.NewInt(1000))                   // c again
	s.SetStorage(contract, slot, common.Hash{0x33}) // storage again

	// revert the nested snapshot: a back to 700, c to 42, storage to 0x22, b still deleted.
	s.RevertToSnapshot(s1)
	if got := s.Balance(a); got.Cmp(big.NewInt(700)) != 0 {
		t.Fatalf("after nested revert, a = %s, want 700", got)
	}
	if got := s.Balance(c); got.Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("after nested revert, c = %s, want 42", got)
	}
	if got := s.GetStorage(contract, slot); got != (common.Hash{0x22}) {
		t.Fatalf("after nested revert, storage = %x, want 0x22", got)
	}

	// revert the outer snapshot: everything back to baseline.
	s.RevertToSnapshot(s0)
	if s.Root() != baseRoot {
		t.Fatalf("after full revert, state root != baseline — journal did not restore exactly")
	}
	if got := s.Balance(b); got.Cmp(big.NewInt(500)) != 0 {
		t.Fatalf("deleted account b not restored: %s, want 500", got)
	}
	if s.Exists(c) {
		t.Fatal("created account c should not exist after revert to before its creation")
	}
	if got := s.GetStorage(contract, slot); got != (common.Hash{0x11}) {
		t.Fatalf("storage not restored: %x, want 0x11", got)
	}
}

// TestJournalCommitKeepsChanges: after DiscardSnapshots (a committed tx) the writes persist
// and the journal is cleared (a later snapshot/revert cannot undo them).
func TestJournalCommitKeepsChanges(t *testing.T) {
	a := common.Address{0xA1}
	s := New()
	s.Credit(a, big.NewInt(1000))

	s.Snapshot()
	s.SubBalance(a, big.NewInt(600)) // a -> 400
	s.DiscardSnapshots()             // commit

	// a new snapshot + revert must not resurrect the pre-commit 1000.
	s2 := s.Snapshot()
	s.Credit(a, big.NewInt(50)) // a -> 450
	s.RevertToSnapshot(s2)
	if got := s.Balance(a); got.Cmp(big.NewInt(400)) != 0 {
		t.Fatalf("after commit+revert, a = %s, want 400 (committed 400, reverted the +50)", got)
	}
}

// TestJournalReverseOrderOnRepeatedWrites: three writes to one account in a
// snapshot scope must undo to the savepoint value, correct only if the journal
// replays newest-first.
func TestJournalReverseOrderOnRepeatedWrites(t *testing.T) {
	a := common.Address{0xA1}
	s := New()
	s.Credit(a, big.NewInt(1000))
	s0 := s.Snapshot()
	s.SubBalance(a, big.NewInt(100)) // 900 (prev 1000)
	s.SubBalance(a, big.NewInt(100)) // 800 (prev 900)
	s.SubBalance(a, big.NewInt(100)) // 700 (prev 800)
	s.RevertToSnapshot(s0)
	if got := s.Balance(a); got.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("repeated-write revert = %s, want 1000 (forward replay would give 800)", got)
	}
}
