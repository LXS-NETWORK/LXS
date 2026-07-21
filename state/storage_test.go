package state

import (
	"math/big"
	"testing"

	"lxs/common"
)

// Storage must fold into the state root: an SSTORE must change the root, or
// contract data is not committed to the block and could be tampered with
// undetectably.
func TestStorageBindsToRoot(t *testing.T) {
	s := New()
	addr := common.Address{0x01}
	s.Credit(addr, big.NewInt(1)) // make the account exist

	root0 := s.Root()

	slot := common.Hash{0x00}
	val := common.Hash{31: 0x2a} // 42 in the low byte
	s.SetStorage(addr, slot, val)

	root1 := s.Root()
	if root0 == root1 {
		t.Fatal("SSTORE did not change the state root — contract data would not be committed to the block")
	}

	// Read-back within the same state.
	if got := s.GetStorage(addr, slot); got != val {
		t.Fatalf("GetStorage returned %s, want %s", got.Hex(), val.Hex())
	}

	// Clearing a slot to zero removes it and the root returns to its prior value
	// (unset == zero).
	s.SetStorage(addr, slot, common.Hash{})
	if s.Root() != root0 {
		t.Fatal("clearing the slot did not restore the original root")
	}
	if got := s.GetStorage(addr, slot); !got.IsZero() {
		t.Fatalf("cleared slot still reads %s", got.Hex())
	}
}

// The state root must not depend on slot write order; Go randomises map
// iteration, so a naive encoder would disagree between runs.
func TestStorageRootIsOrderIndependent(t *testing.T) {
	build := func(order []int) common.Hash {
		s := New()
		addr := common.Address{0x02}
		s.Credit(addr, big.NewInt(1))
		for _, i := range order {
			s.SetStorage(addr, common.Hash{byte(i)}, common.Hash{31: byte(i + 1)})
		}
		return s.Root()
	}
	forward := build([]int{1, 2, 3, 4, 5})
	backward := build([]int{5, 4, 3, 2, 1})
	if forward != backward {
		t.Fatal("state root depends on storage write order — the encoder is not sorting")
	}
}
