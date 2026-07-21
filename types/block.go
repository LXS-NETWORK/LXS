package types

import (
	"sync/atomic"

	"lxs/common"
)

// IntrinsicGas is the base per-transaction cost (a plain transfer), charged before
// execution so spamming zero-work txs still costs money. Data-carrying and creation
// txs cost more — see IntrinsicGasFor.
const IntrinsicGas uint64 = 21000

const (
	// TxGasContractCreation is the base cost of a creation tx: the 21000 base plus
	// the 32000 creation surcharge.
	TxGasContractCreation uint64 = 53000
	// TxDataNonZeroGas / TxDataZeroGas price each calldata byte (EIP-2028, Istanbul).
	// The block gas limit bounds gas, not bytes, so pricing calldata is the defense
	// against filling blocks with cheap maximal-data txs every node must store and gossip.
	TxDataNonZeroGas uint64 = 16
	TxDataZeroGas    uint64 = 4
)

// IntrinsicGasFor is the pre-execution cost of a tx: the base (21000, or 53000 for a
// creation) plus a per-byte calldata charge. Matches go-ethereum so a tx priced for
// mainnet is priced the same here.
func IntrinsicGasFor(data []byte, isCreate bool) uint64 {
	gas := IntrinsicGas
	if isCreate {
		gas = TxGasContractCreation
	}
	for _, b := range data {
		if b == 0 {
			gas += TxDataZeroGas
		} else {
			gas += TxDataNonZeroGas
		}
	}
	return gas
}

// Header is what a light client downloads and what consensus commits to: the
// block's contents via TxRoot and the world state via StateRoot.
type Header struct {
	ParentHash  common.Hash    `json:"parentHash"`
	Height      uint64         `json:"height"`
	Timestamp   int64          `json:"timestamp"` // unix millis
	TxRoot      common.Hash    `json:"txRoot"`
	ReceiptRoot common.Hash    `json:"receiptRoot"`
	StateRoot   common.Hash    `json:"stateRoot"`
	GasUsed     uint64         `json:"gasUsed"`
	GasLimit    uint64         `json:"gasLimit"`
	Proposer    common.Address `json:"proposer"`

	// Difficulty is the target the Nonce had to beat, committed so a validator
	// can recompute it and check the proof. Derived from the parent, not chosen:
	// a block whose Difficulty disagrees is rejected, so a miner cannot lower it.
	Difficulty uint64 `json:"difficulty"`

	// Nonce is the proof of work: the one field a miner grinds. It is inside the
	// hash, so the miner searches Nonce until Hash() falls under target.
	Nonce uint64 `json:"nonce"`

	hash atomic.Value
}

// Hash is the block identity. The header commits to TxRoot and StateRoot, so
// hashing it alone transitively commits to the whole block and state.
func (h *Header) Hash() common.Hash {
	if v := h.hash.Load(); v != nil {
		return v.(common.Hash)
	}
	e := common.NewEncoder()
	e.Raw(h.ParentHash.Bytes())
	e.Uint64(h.Height)
	e.Int64(h.Timestamp)
	e.Raw(h.TxRoot.Bytes())
	e.Raw(h.ReceiptRoot.Bytes())
	e.Raw(h.StateRoot.Bytes())
	e.Uint64(h.GasUsed)
	e.Uint64(h.GasLimit)
	e.Raw(h.Proposer.Bytes())
	e.Uint64(h.Difficulty)
	e.Uint64(h.Nonce)
	hash := common.Keccak256(e.Done())
	h.hash.Store(hash)
	return hash
}

// InvalidateHash clears the cached hash so the next Hash() recomputes. Only the
// miner needs it, mutating Nonce each try. Not safe for concurrent use: the
// miner owns its header exclusively while grinding.
func (h *Header) InvalidateHash() { h.hash = atomic.Value{} }

type Block struct {
	Header *Header        `json:"header"`
	Txs    []*Transaction `json:"txs"`
}

func (b *Block) Hash() common.Hash { return b.Header.Hash() }
func (b *Block) Height() uint64    { return b.Header.Height }

// VerifyTxRoot rejects a header whose body does not match its TxRoot.
func (b *Block) VerifyTxRoot() bool {
	return TxRoot(b.Txs) == b.Header.TxRoot
}
