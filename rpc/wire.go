package rpc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"lxs/common"
	"lxs/types"
)

// Wire format lives here, not on the consensus types. types.Transaction is
// hashed and signed via the canonical binary encoder in common/. JSON tags on
// that struct would invite json.Marshal for hashing, and map ordering would
// silently break consensus. Rule: JSON never touches a hash.

// Quantity is an integer on the wire as a 0x-prefixed hex string, not a JSON
// number: JSON numbers are IEEE-754 doubles in most parsers (including
// JavaScript), so a balance of 10^24 wei silently loses precision. This is why
// Ethereum's RPC is hex strings throughout.
type Quantity struct{ *big.Int }

func Q(i *big.Int) Quantity { return Quantity{i} }
func QU(v uint64) Quantity  { return Quantity{new(big.Int).SetUint64(v)} }
func (q Quantity) U64() uint64 {
	if q.Int == nil {
		return 0
	}
	return q.Int.Uint64()
}

func (q Quantity) MarshalJSON() ([]byte, error) {
	if q.Int == nil {
		return []byte(`"0x0"`), nil
	}
	return []byte(`"0x` + q.Int.Text(16) + `"`), nil
}

func (q *Quantity) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return errors.New("rpc: quantity must be a hex string, not a number")
	}
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return errors.New("rpc: empty quantity")
	}
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return fmt.Errorf("rpc: bad hex quantity %q", s)
	}
	q.Int = v
	return nil
}

// Data is a byte slice on the wire as 0x-prefixed hex.
type Data []byte

func (d Data) MarshalJSON() ([]byte, error) {
	return []byte(`"0x` + hex.EncodeToString(d) + `"`), nil
}

func (d *Data) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return err
	}
	*d = b
	return nil
}

// TxArgs is a signed transaction as it arrives over the wire. There is no
// `from`: the server recovers it, so a client cannot lie about its identity by
// setting a field.
type TxArgs struct {
	ChainID  Quantity        `json:"chainId"`
	Nonce    Quantity        `json:"nonce"`
	To       *common.Address `json:"to"`
	Value    Quantity        `json:"value"`
	GasLimit Quantity        `json:"gasLimit"`
	GasPrice Quantity        `json:"gasPrice"`
	Data     Data            `json:"data"`
	Sig      Data            `json:"sig"`
}

func FromTx(tx *types.Transaction) *TxArgs {
	return &TxArgs{
		ChainID:  QU(tx.ChainID),
		Nonce:    QU(tx.Nonce),
		To:       tx.To,
		Value:    Q(tx.Value),
		GasLimit: QU(tx.GasLimit),
		GasPrice: Q(tx.GasPrice),
		Data:     tx.Data,
		Sig:      tx.Sig,
	}
}

func (a *TxArgs) ToTx() *types.Transaction {
	return &types.Transaction{
		ChainID:  a.ChainID.U64(),
		Nonce:    a.Nonce.U64(),
		To:       a.To,
		Value:    a.Value.Int,
		GasLimit: a.GasLimit.U64(),
		GasPrice: a.GasPrice.Int,
		Data:     a.Data,
		Sig:      a.Sig,
	}
}

// TxResult is a transaction as returned by the node, with the derived
// fields the caller cannot compute cheaply themselves.
type TxResult struct {
	Hash        common.Hash     `json:"hash"`
	From        common.Address  `json:"from"` // recovered, not claimed
	To          *common.Address `json:"to"`
	Nonce       Quantity        `json:"nonce"`
	Value       Quantity        `json:"value"`
	GasLimit    Quantity        `json:"gasLimit"`
	GasPrice    Quantity        `json:"gasPrice"`
	Data        Data            `json:"data"`
	BlockHash   *common.Hash    `json:"blockHash"`   // null while pending
	BlockHeight *Quantity       `json:"blockHeight"` // null while pending
	TxIndex     *Quantity       `json:"txIndex"`     // null while pending
}

// ReceiptResult is the consensus receipt plus location metadata. The metadata is
// here, not in types.Receipt, because a receipt is hashed into its block's header
// and cannot contain that block's own hash without being circular.
type ReceiptResult struct {
	TxHash            common.Hash     `json:"txHash"`
	Status            Quantity        `json:"status"`
	GasUsed           Quantity        `json:"gasUsed"`
	CumulativeGasUsed Quantity        `json:"cumulativeGasUsed"`
	BlockHash         common.Hash     `json:"blockHash"`
	BlockHeight       Quantity        `json:"blockHeight"`
	TxIndex           Quantity        `json:"txIndex"`
	From              common.Address  `json:"from"`
	To                *common.Address `json:"to"`
	// ContractAddress is set only for a deployment (To == null): the address of
	// the freshly created contract, which the deployer needs to reach it.
	ContractAddress *common.Address `json:"contractAddress"`
	// Logs are the events the transaction emitted, in order.
	Logs []LogResult `json:"logs"`
}

// LogResult is one emitted event, rendered for the wire (hex data, not base64).
type LogResult struct {
	Address common.Address `json:"address"`
	Topics  []common.Hash  `json:"topics"`
	Data    Data           `json:"data"`
}

// CallArgs is a read-only eth_call. There is no signature and no nonce: nothing
// is committed, so nothing needs to be authorised. From defaults to the zero
// address when omitted.
type CallArgs struct {
	From *common.Address `json:"from"`
	To   common.Address  `json:"to"`
	Data Data            `json:"data"`
	Gas  *Quantity       `json:"gas"`
}

type HeaderResult struct {
	Hash        common.Hash    `json:"hash"`
	ParentHash  common.Hash    `json:"parentHash"`
	Height      Quantity       `json:"height"`
	Timestamp   Quantity       `json:"timestamp"`
	TxRoot      common.Hash    `json:"txRoot"`
	ReceiptRoot common.Hash    `json:"receiptRoot"`
	StateRoot   common.Hash    `json:"stateRoot"`
	GasUsed     Quantity       `json:"gasUsed"`
	GasLimit    Quantity       `json:"gasLimit"`
	Proposer    common.Address `json:"proposer"`
}

type BlockResult struct {
	HeaderResult
	// Either hashes or full objects, depending on the fullTx argument.
	Txs []interface{} `json:"txs"`
}
