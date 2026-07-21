package common

// Log is an event a contract emits with LOG0..LOG4. Consensus data: logs are
// hashed into the receipt root, so a light client can trust a log plus a Merkle
// proof without re-executing the block. A node-local log could be forged; one
// committed to the receipt root cannot.
//
// Topics are the indexed parameters (at most four, one per LOGn), by convention
// with keccak256(event signature) first; Data is the non-indexed payload. The
// split lets a node filter by topic without decoding every event body, which is
// what eth_getLogs relies on.
type Log struct {
	Address Address
	Topics  []Hash
	Data    []byte
}

// EncodeInto folds the log into a hashing encoder. Field order and the explicit
// topic count are fixed so every node derives the same receipt root.
func (l *Log) EncodeInto(e *Encoder) {
	e.Raw(l.Address[:])
	e.Uint64(uint64(len(l.Topics)))
	for i := range l.Topics {
		e.Raw(l.Topics[i][:])
	}
	e.Bytes(l.Data) // length-prefixed: data is variable-length
}
