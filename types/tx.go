package types

import (
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"

	"lxs/common"
	"lxs/crypto"
)

// TxType is the transaction envelope version: EIP-2718 in one byte. It lives
// inside the signing hash, so it cannot be added later without a hard fork.
// Adding a type is then a rules change, not an encoding change, which is the
// difference between a coordinated upgrade and a chain split.
type TxType uint8

const (
	// TxTypeTransfer is the native LXS transaction (value transfer and contract
	// calls), signed with the native signing hash.
	TxTypeTransfer TxType = 0x00

	// TxTypeEthLegacy is an internal marker for a transaction that arrived as an
	// Ethereum legacy (EIP-155) payload via eth_sendRawTransaction. Not an
	// EIP-2718 envelope byte (legacy txs have none): it selects the Ethereum
	// signing-hash and tx-hash rules so a MetaMask signature recovers the right
	// sender. It rides in the JSON `type` field, so every node agrees after
	// gossip and storage.
	TxTypeEthLegacy TxType = 0x64
)

// isKnownTxType is a whitelist. Unknown types are rejected, never ignored:
// an old node skipping a type a new node executes computes a different state
// root from the same block, splitting the chain.
func isKnownTxType(t TxType) bool {
	return t == TxTypeTransfer || t == TxTypeEthLegacy
}

var (
	ErrTxType        = errors.New("types: unknown transaction type")
	ErrTxUnsigned    = errors.New("types: transaction is unsigned")
	ErrBadSignature  = errors.New("types: invalid signature")
	ErrNegativeValue = errors.New("types: negative value")
	ErrChainID       = errors.New("types: wrong chain id")
)

// Transaction is a signed state-transition request. There is no From field:
// the sender is recovered from the signature. A field is a claim; a recovered
// key is a proof.
type Transaction struct {
	// Type is the first field in the signing hash, so the signature commits to
	// the shape it signed. A type byte outside the signature is one an attacker
	// rewrites in flight while it still verifies.
	Type     TxType          `json:"type"`
	ChainID  uint64          `json:"chainId"`
	Nonce    uint64          `json:"nonce"`
	To       *common.Address `json:"to"` // nil => contract creation
	Value    *big.Int        `json:"value"`
	GasLimit uint64          `json:"gasLimit"`
	GasPrice *big.Int        `json:"gasPrice"`
	Data     []byte          `json:"data"`
	Sig      []byte          `json:"sig"` // 65-byte compact recoverable

	// caches. atomic.Value: a tx is shared across mempool and block builder.
	hash   atomic.Value
	sender atomic.Value
}

func NewTransaction(chainID, nonce uint64, to common.Address, value *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) *Transaction {
	cp := to
	return &Transaction{
		Type:     TxTypeTransfer,
		ChainID:  chainID,
		Nonce:    nonce,
		To:       &cp,
		Value:    new(big.Int).Set(value),
		GasLimit: gasLimit,
		GasPrice: new(big.Int).Set(gasPrice),
		Data:     append([]byte(nil), data...),
	}
}

// SigningHash is the digest that gets signed. It excludes Sig (cannot sign a
// signature) and includes ChainID (so a tx for chain A is not replayable on
// chain B; EIP-155).
func (tx *Transaction) SigningHash() common.Hash {
	// A legacy tx is signed over the EIP-155 RLP, not the native encoding.
	if tx.Type == TxTypeEthLegacy {
		return tx.ethSigningHash()
	}
	e := common.NewEncoder()
	// Type first: domain separation, so a signature for one type cannot be
	// replayed as another.
	e.Uint64(uint64(tx.Type))
	e.Uint64(tx.ChainID)
	e.Uint64(tx.Nonce)
	e.OptionalAddress(tx.To)
	e.BigInt(tx.Value)
	e.Uint64(tx.GasLimit)
	e.BigInt(tx.GasPrice)
	e.Bytes(tx.Data)
	return common.Keccak256(e.Done())
}

// Hash identifies the transaction and includes the signature, so two
// signatures over the same payload are two different txs.
func (tx *Transaction) Hash() common.Hash {
	if v := tx.hash.Load(); v != nil {
		return v.(common.Hash)
	}
	var h common.Hash
	if tx.Type == TxTypeEthLegacy && len(tx.Sig) == 65 {
		// The identity wallets and explorers expect: keccak256 of the raw
		// signed RLP. Only a well-formed 65-byte signature can be raw-encoded;
		// a malformed one (a hostile gossip tx can set any type+sig) falls to the
		// native branch below, which is length-safe, so Hash() can never panic
		// (Hash runs on untrusted input BEFORE SanityCheck, during dedup).
		h = tx.ethRawHash()
	} else {
		e := common.NewEncoder()
		e.Raw(tx.SigningHash().Bytes())
		e.Bytes(tx.Sig)
		h = common.Keccak256(e.Done())
	}
	tx.hash.Store(h)
	return h
}

func (tx *Transaction) Sign(key *crypto.PrivateKey) error {
	sig, err := crypto.Sign(tx.SigningHash(), key)
	if err != nil {
		return err
	}
	tx.Sig = sig
	tx.hash = atomic.Value{}
	tx.sender = atomic.Value{}
	return nil
}

// Sender recovers and caches the signing address.
func (tx *Transaction) Sender() (common.Address, error) {
	if v := tx.sender.Load(); v != nil {
		return v.(common.Address), nil
	}
	if len(tx.Sig) != crypto.SignatureLength {
		return common.ZeroAddress, ErrTxUnsigned
	}
	addr, err := crypto.RecoverAddress(tx.SigningHash(), tx.Sig)
	if err != nil {
		return common.ZeroAddress, ErrBadSignature
	}
	tx.sender.Store(addr)
	return addr, nil
}

// SanityCheck is stateless validation. Cheap checks first: it runs on every tx
// arriving from the network, so it is a DoS surface.
func (tx *Transaction) SanityCheck(chainID uint64) error {
	// Type first, before any field is trusted.
	if !isKnownTxType(tx.Type) {
		return fmt.Errorf("%w: 0x%02x", ErrTxType, uint8(tx.Type))
	}
	if tx.ChainID != chainID {
		return ErrChainID
	}
	if tx.Value == nil || tx.Value.Sign() < 0 {
		return ErrNegativeValue
	}
	if tx.GasPrice == nil || tx.GasPrice.Sign() < 0 {
		return errors.New("types: negative gas price")
	}
	if tx.GasLimit < IntrinsicGasFor(tx.Data, tx.To == nil) {
		return errors.New("types: gas limit below intrinsic cost")
	}
	if len(tx.Sig) != crypto.SignatureLength {
		return ErrTxUnsigned
	}
	// Recovery is the expensive part (~50us), so it goes last.
	_, err := tx.Sender()
	return err
}

// Cost is the maximum an account must be able to afford: value + gas.
func (tx *Transaction) Cost() *big.Int {
	fee := new(big.Int).Mul(new(big.Int).SetUint64(tx.GasLimit), tx.GasPrice)
	return fee.Add(fee, tx.Value)
}
