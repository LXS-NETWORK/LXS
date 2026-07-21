package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/state"
	"lxs/store"
)

// Contract storage must survive a restart: loadState rebuilds the slots, not just
// balance and nonce. Otherwise a node comes back having forgotten every contract's
// data, including token balances.
func TestLoadStateRestoresStorage(t *testing.T) {
	db := store.NewMemory()

	addr := common.Address{0xAB}
	slot := common.Hash{0x01}
	val := common.Hash{31: 0x2a}

	// Write an account with storage straight to disk, as a committed block would.
	acc := &state.Account{
		Nonce:   1,
		Balance: big.NewInt(100),
		Storage: map[common.Hash]common.Hash{slot: val},
	}
	data, err := encodeJSON(acc)
	if err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	b.Put(accountKey(addr), data)
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}

	// Reload the whole world from disk, as a restart does.
	s, err := loadState(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.GetStorage(addr, slot); got != val {
		t.Fatalf("storage lost across restart: got %s, want %s", got.Hex(), val.Hex())
	}
	if got := s.Balance(addr).Int64(); got != 100 {
		t.Fatalf("balance lost across restart: %d", got)
	}
}
