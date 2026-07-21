package types

import "lxs/common"

// Receipt status codes.
const (
	ReceiptFailed  uint64 = 0
	ReceiptSuccess uint64 = 1
)

// Receipt is the consensus receipt: the part hashed into the header via
// ReceiptRoot.
//
// BlockHash, BlockHeight and TxIndex are deliberately absent: a receipt is
// hashed into the block it describes, so it cannot contain that block's hash.
// Block metadata is derived at the RPC layer instead, not committed.
type Receipt struct {
	Status            uint64 `json:"status"`
	CumulativeGasUsed uint64 `json:"cumulativeGasUsed"`
	GasUsed           uint64 `json:"gasUsed"`

	// Logs are the events the transaction emitted. Consensus data, hashed into
	// the receipt root so a light client can prove an event happened without
	// trusting the serving node. (A bloom filter is a later eth_getLogs
	// optimisation; the logs themselves are the commitment.)
	Logs []*common.Log `json:"logs"`
}

func (r *Receipt) encode() []byte {
	e := common.NewEncoder()
	e.Uint64(r.Status)
	e.Uint64(r.CumulativeGasUsed)
	e.Uint64(r.GasUsed)
	// Explicit log count prevents a no-log receipt colliding with one whose
	// first log encodes to nothing.
	e.Uint64(uint64(len(r.Logs)))
	for _, l := range r.Logs {
		l.EncodeInto(e)
	}
	return e.Done()
}

// ReceiptRoot commits to a block's execution results. The state root proves
// final balances but not why: without a receipt root a node could lie about
// whether a transaction succeeded and a light client could not check.
// Load-bearing once a contract can revert, consume gas, and still be included.
func ReceiptRoot(receipts []*Receipt) common.Hash {
	leaves := make([][]byte, len(receipts))
	for i, r := range receipts {
		leaves[i] = r.encode()
	}
	return MerkleRoot(leaves)
}
