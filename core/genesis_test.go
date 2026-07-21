package core

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/state"
	"lxs/store"
	"lxs/types"
)

func TestGenesisSupplyMismatchIsRefused(t *testing.T) {
	a, b := common.Address{0x01}, common.Address{0x02}
	g := &Genesis{
		Name: "LXS", ChainID: 1337, Timestamp: 1700000000000, GasLimit: 30_000_000,
		Alloc: map[common.Address]*BigStr{
			a: {Int: common.LXS(80_000_000)},
			b: {Int: common.LXS(20_000_000)},
		},
		TotalSupply: &BigStr{Int: common.LXS(100_000_000)},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("a correct genesis was refused: %v", err)
	}

	// One digit wrong, in the one block that can never be reorged.
	g.Alloc[b] = &BigStr{Int: common.LXS(2_000_000)}
	if err := g.Validate(); err == nil {
		t.Fatal("allocations that do not sum to totalSupply were accepted")
	}

	// The chain must refuse to open on it.
	if _, err := NewBlockchain(store.NewMemory(), g, Options{}); err == nil {
		t.Fatal("a chain opened on an inconsistent genesis")
	}
}

func TestGenesisRejectsZeroChainID(t *testing.T) {
	g := &Genesis{Name: "LXS", ChainID: 0, GasLimit: 30_000_000}
	if err := g.Validate(); err == nil {
		t.Fatal("chain id 0 was accepted — every signature would be replayable")
	}
}

func TestGenesisRejectsGasLimitBelowMinimum(t *testing.T) {
	// A genesis below MinBlockGasLimit can never produce a valid block 1, and
	// genesis cannot be fixed later — so it must be rejected up front, not left to
	// brick the chain at launch.
	g := &Genesis{Name: "LXS", ChainID: 1337, GasLimit: state.MinBlockGasLimit - 1}
	if err := g.Validate(); err == nil {
		t.Fatal("a genesis gas limit below MinBlockGasLimit was accepted — the chain would be unminable")
	}
}

func TestGenesisRejectsNegativeAllocation(t *testing.T) {
	g := &Genesis{
		Name: "LXS", ChainID: 1337, GasLimit: 30_000_000,
		Alloc: map[common.Address]*BigStr{{0x01}: {Int: big.NewInt(-1)}},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("a negative genesis allocation was accepted")
	}
}

func TestUnstatedTotalSupplyIsNotChecked(t *testing.T) {
	g := &Genesis{
		Name: "LXS", ChainID: 1337, GasLimit: 30_000_000,
		Alloc: map[common.Address]*BigStr{{0x01}: {Int: common.LXS(5)}},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("omitting totalSupply should be allowed: %v", err)
	}
}

// The genesis allocation is real money and must survive into the state.
func TestGenesisSupplyMatchesState(t *testing.T) {
	founder, treasury := common.Address{0xf0}, common.Address{0x71}
	g := &Genesis{
		Name: "LXS", ChainID: 1337, Timestamp: 1700000000000, GasLimit: 30_000_000,
		Alloc: map[common.Address]*BigStr{
			founder:  {Int: common.LXS(20_000_000)},
			treasury: {Int: common.LXS(80_000_000)},
		},
		TotalSupply: &BigStr{Int: common.LXS(100_000_000)},
	}
	bc, err := NewBlockchain(store.NewMemory(), g, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st := bc.StateSnapshot()

	sum := new(big.Int)
	for _, acc := range st.Accounts() {
		sum.Add(sum, acc.Balance)
	}
	if sum.Cmp(common.LXS(100_000_000)) != 0 {
		t.Fatalf("genesis state supply: got %s want %s", sum, common.LXS(100_000_000))
	}
	if st.Balance(founder).Cmp(common.LXS(20_000_000)) != 0 {
		t.Fatal("founder allocation did not land")
	}
}

// TestChainIDSeparatesOtherwiseIdenticalNets: two genesis configs that differ ONLY
// in ChainID must produce different genesis hashes AND different empty (tx-less)
// block hashes. Otherwise a testnet cloned from mainnet's allocation would mint
// byte-identical early blocks, replayable from one chain onto the other. EIP-155
// covers transactions; this covers the block itself.
func TestChainIDSeparatesOtherwiseIdenticalNets(t *testing.T) {
	mk := func(chainID uint64) *Genesis {
		return &Genesis{
			Name: "LXS", ChainID: chainID, Timestamp: 1700000000000, GasLimit: 30_000_000,
			Alloc:       map[common.Address]*BigStr{{0x01}: {Int: common.LXS(100_000_000)}},
			TotalSupply: &BigStr{Int: common.LXS(100_000_000)},
		}
	}
	_, g1 := mk(1).Build()
	_, g2 := mk(2).Build()

	if g1.Hash() == g2.Hash() {
		t.Fatal("two nets with the same alloc but different ChainID share a genesis hash — empty blocks are replayable")
	}

	// The separation propagates: a height-1 empty block built on each genesis also
	// differs, purely through the parent linkage.
	empty := func(parent *types.Block) common.Hash {
		h := &types.Header{
			ParentHash: parent.Hash(), Height: 1, Timestamp: parent.Header.Timestamp + 1,
			TxRoot: types.TxRoot(nil), ReceiptRoot: types.ReceiptRoot(nil),
			StateRoot: parent.Header.StateRoot, GasLimit: parent.Header.GasLimit,
		}
		return h.Hash()
	}
	if empty(g1) == empty(g2) {
		t.Fatal("height-1 empty blocks collide across nets — the ChainID did not propagate past genesis")
	}

	// Same ChainID must be deterministic (no accidental per-build nonce).
	_, g1b := mk(1).Build()
	if g1.Hash() != g1b.Hash() {
		t.Fatal("genesis hash is not deterministic for a fixed ChainID")
	}
}
