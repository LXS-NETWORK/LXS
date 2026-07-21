package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"

	"lxs/common"
	"lxs/state"
	"lxs/types"
)

// Genesis is block zero, agreed by convention: every node must load a
// byte-identical genesis or they are on different chains.
//
// Name is hashed into nothing; it is a human label. The chain's identity is
// (ChainID, genesis hash), both checked on every database open and signature.
type Genesis struct {
	Name string `json:"name"`

	// TotalSupply is the declared sum of every genesis allocation, checked against
	// the allocations by Validate. Not a cap (nothing mints at genesis); a
	// typo-catcher for the most expensive typo available, the money in block zero.
	TotalSupply *BigStr                    `json:"totalSupply"`
	ChainID     uint64                     `json:"chainId"`
	Timestamp   int64                      `json:"timestamp"`
	GasLimit    uint64                     `json:"gasLimit"`
	Alloc       map[common.Address]*BigStr `json:"alloc"`

	// Difficulty is the starting mining difficulty, a consensus parameter hashed
	// into the genesis header so every node adjusts from the same base. 0 = floor.
	Difficulty uint64 `json:"difficulty"`

	// TreasuryReward / TreasuryRewardBps are the block-reward split: TreasuryRewardBps
	// basis points of every block reward go to TreasuryReward, the proposer keeps the
	// rest. Both are consensus parameters read from genesis, so every node computes
	// the same split or its state root is rejected. Zero/zero (default) = proposer
	// keeps 100%, byte-for-byte identical to a genesis without the field. If
	// TreasuryReward is common.BurnAddress the cut is destroyed, not credited (folds
	// into the consensus burn total, needs no key).
	TreasuryReward    common.Address `json:"treasuryReward,omitempty"`
	TreasuryRewardBps uint64         `json:"treasuryRewardBps,omitempty"`
}

// The founder's stake is a plain genesis allocation: init's -founder-pct puts the
// founder's address in Alloc. A genesis allocation is the pre-mine; there is no
// per-block founder cut.

// BigStr is a *big.Int that survives JSON as a decimal string. Balances exceed
// float64 precision, and encoding/json destroys them as numbers.
type BigStr struct{ *big.Int }

func (b BigStr) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }
func (b *BigStr) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return json.Unmarshal(data, &struct{}{})
	}
	b.Int = v
	return nil
}

func (g *Genesis) Save(path string) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func LoadGenesis(path string) (*Genesis, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g Genesis
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// Validate checks the genesis is internally consistent. Genesis can never be
// reorged, so a mistake here means restarting the network, not fixing a bug.
func (g *Genesis) Validate() error {
	if g.ChainID == 0 {
		// Chain id 0 makes every signature valid on a chain declaring no id: the
		// replay hole EIP-155 closes.
		return errors.New("core: chain id must not be zero")
	}
	if g.GasLimit < state.MinBlockGasLimit {
		// Block 1 must clear MinBlockGasLimit and stay within parent/1024 of genesis;
		// a genesis below the floor can never produce a valid block, and genesis
		// cannot be fixed later.
		return fmt.Errorf("core: gas limit %d is below the minimum %d — no valid block could follow genesis",
			g.GasLimit, state.MinBlockGasLimit)
	}
	if g.TreasuryRewardBps > 10000 {
		return fmt.Errorf("core: treasuryRewardBps %d exceeds 10000 (100%%)", g.TreasuryRewardBps)
	}
	// Both-or-nothing: a bps with no destination would pay the zero address; a
	// destination with no bps does nothing. Neither is fixable after genesis.
	if (g.TreasuryRewardBps > 0) != (g.TreasuryReward != (common.Address{})) {
		return errors.New("core: treasuryRewardBps and treasuryReward must both be set or both be empty")
	}

	sum := new(big.Int)
	for addr, bal := range g.Alloc {
		if bal == nil || bal.Int == nil {
			return fmt.Errorf("core: nil allocation for %s", addr.Hex())
		}
		if bal.Int.Sign() < 0 {
			return fmt.Errorf("core: negative allocation for %s", addr.Hex())
		}
		sum.Add(sum, bal.Int)
	}

	if g.TotalSupply == nil || g.TotalSupply.Int == nil {
		// Optional: an unstated total is not checked; a wrong one must fail.
		return nil
	}
	if sum.Cmp(g.TotalSupply.Int) != 0 {
		return fmt.Errorf("core: allocations sum to %s but totalSupply says %s — "+
			"one of the two is wrong, and genesis cannot be fixed later",
			sum, g.TotalSupply.Int)
	}
	return nil
}

// Supply returns the sum of all genesis allocations, the founder's stake included.
func (g *Genesis) Supply() *big.Int {
	sum := new(big.Int)
	for _, bal := range g.Alloc {
		if bal != nil && bal.Int != nil {
			sum.Add(sum, bal.Int)
		}
	}
	return sum
}

func (g *Genesis) Build() (*state.State, *types.Block) {
	s := state.New()
	for addr, bal := range g.Alloc {
		s.Credit(addr, bal.Int)
	}
	diff := g.Difficulty
	if diff == 0 {
		diff = MinDifficulty
	}
	header := &types.Header{
		// Bind genesis — and thus, via ParentHash, EVERY block that descends from it —
		// to this network's ChainID. See chainTag.
		ParentHash:  chainTag(g.ChainID),
		Height:      0,
		Timestamp:   g.Timestamp,
		TxRoot:      types.TxRoot(nil),
		ReceiptRoot: types.ReceiptRoot(nil),
		StateRoot:   s.Root(),
		GasUsed:     0,
		GasLimit:    g.GasLimit,
		Proposer:    common.ZeroAddress,
		Difficulty:  diff,
		// Nonce stays 0: genesis is agreed by convention, not mined.
	}
	return s, &types.Block{Header: header, Txs: nil}
}

// chainTag derives the genesis block's ParentHash from the ChainID, so two networks
// that share the same allocation still produce different genesis hashes — and,
// because every block commits to its parent, different hashes for every descendant
// block including tx-less ones.
//
// The failure mode without it: clone mainnet's genesis alloc into a "testnet" with a
// different ChainID and the two chains mint byte-identical genesis blocks and
// byte-identical EMPTY (tx-less) early blocks. EIP-155 scopes transactions to a
// chain, but a block carrying no transactions carried no chain identity at all, so
// an early empty block mined on one net was a valid block on the other. Genesis has
// no real parent, so its ParentHash slot is free to hold this identity tag; a
// versioned prefix lets the derivation change without colliding with a raw hash.
func chainTag(chainID uint64) common.Hash {
	e := common.NewEncoder()
	e.Raw([]byte("lxs/genesis/v1"))
	e.Uint64(chainID)
	return common.Keccak256(e.Done())
}
