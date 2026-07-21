package types

import (
	"errors"
	"math/big"

	"lxs/common"
	"lxs/rlp"
)

// Bridge from an Ethereum legacy (EIP-155) transaction, the bytes MetaMask puts
// on the wire, to the internal Transaction. Two things must line up for a
// MetaMask signature to verify:
//
//   1. Signing hash: keccak256(rlp([nonce, gasPrice, gasLimit, to, value, data,
//      chainId, 0, 0])). The sender is recovered against exactly this.
//   2. Tx hash: keccak256 of the raw signed RLP, so a wallet finds its receipt.
//
// The low-s rule is not an obstacle: EIP-2 already requires low-s, which is what
// MetaMask produces.

var (
	ErrEthFieldCount = errors.New("types: legacy tx must be a 9-field RLP list")
	ErrEthV          = errors.New("types: unsupported signature v value")
	ErrEthField      = errors.New("types: malformed legacy tx field")
)

// addrBytes returns the 20-byte address, or empty for contract creation.
func addrBytes(to *common.Address) []byte {
	if to == nil {
		return nil
	}
	return to[:]
}

// ethSigningHash is the EIP-155 digest the sender signed.
func (tx *Transaction) ethSigningHash() common.Hash {
	return common.Keccak256(rlp.List(
		rlp.Uint(tx.Nonce),
		rlp.Big(tx.GasPrice),
		rlp.Uint(tx.GasLimit),
		rlp.Bytes(addrBytes(tx.To)),
		rlp.Big(tx.Value),
		rlp.Bytes(tx.Data),
		rlp.Uint(tx.ChainID),
		rlp.Bytes(nil), // EIP-155 trailing zeros
		rlp.Bytes(nil),
	))
}

// EncodeEthRaw reconstructs the full signed RLP (v, r, s): the bytes MetaMask
// put on the wire, re-broadcast as-is. v is rebuilt from the chain id and the
// recovery id in the compact signature.
func (tx *Transaction) EncodeEthRaw() []byte {
	// Total on any input: indexing a non-65-byte signature would panic, and this is
	// reached from Hash() on untrusted gossip txs before validation. A malformed sig
	// yields no raw encoding; callers treat nil as "not an eth-legacy raw tx".
	if len(tx.Sig) != 65 {
		return nil
	}
	recid := uint64(tx.Sig[0] - 27)
	v := tx.ChainID*2 + 35 + recid
	r := new(big.Int).SetBytes(tx.Sig[1:33])
	s := new(big.Int).SetBytes(tx.Sig[33:65])
	return rlp.List(
		rlp.Uint(tx.Nonce),
		rlp.Big(tx.GasPrice),
		rlp.Uint(tx.GasLimit),
		rlp.Bytes(addrBytes(tx.To)),
		rlp.Big(tx.Value),
		rlp.Bytes(tx.Data),
		rlp.Uint(v),
		rlp.Big(r),
		rlp.Big(s),
	)
}

// ethRawHash is the transaction's Ethereum identity: keccak256 of the raw
// signed RLP.
func (tx *Transaction) ethRawHash() common.Hash {
	return common.Keccak256(tx.EncodeEthRaw())
}

// ParseEthLegacyTx decodes a raw EIP-155 signed transaction (an
// eth_sendRawTransaction body) into an internal Transaction. Input is untrusted:
// every field length is validated and the signature must recover a sender
// before returning.
func ParseEthLegacyTx(raw []byte) (*Transaction, error) {
	items, err := rlp.DecodeList(raw)
	if err != nil {
		return nil, err
	}
	if len(items) != 9 {
		return nil, ErrEthFieldCount
	}

	nonce, err := bytesToUint64(items[0])
	if err != nil {
		return nil, err
	}
	gasPrice := new(big.Int).SetBytes(items[1])
	gasLimit, err := bytesToUint64(items[2])
	if err != nil {
		return nil, err
	}
	toBytes, value, data := items[3], new(big.Int).SetBytes(items[4]), items[5]
	v := new(big.Int).SetBytes(items[6])
	r, s := items[7], items[8]

	// EIP-155: v = chainId*2 + 35 + recid. Pre-155: v = 27 + recid (chain id 0).
	// Anything else is rejected.
	var chainID, recid uint64
	switch vv := v.Uint64(); {
	case vv == 27 || vv == 28:
		recid, chainID = vv-27, 0
	case vv >= 35:
		x := vv - 35
		recid = x & 1
		chainID = (x - recid) / 2
	default:
		return nil, ErrEthV
	}

	var to *common.Address
	switch len(toBytes) {
	case 0: // contract creation
	case common.AddressLength:
		var a common.Address
		copy(a[:], toBytes)
		to = &a
	default:
		return nil, ErrEthField
	}

	if len(r) > 32 || len(s) > 32 || len(r) == 0 || len(s) == 0 {
		return nil, ErrEthField
	}
	// Compact signature: [27+recid | R(32) | S(32)], left-padded.
	sig := make([]byte, 65)
	sig[0] = byte(27 + recid)
	copy(sig[1+(32-len(r)):33], r)
	copy(sig[33+(32-len(s)):65], s)

	tx := &Transaction{
		Type:     TxTypeEthLegacy,
		ChainID:  chainID,
		Nonce:    nonce,
		To:       to,
		Value:    value,
		GasLimit: gasLimit,
		GasPrice: gasPrice,
		Data:     data,
		Sig:      sig,
	}
	// Recover now: proves the signature is well-formed and low-s, and warms the
	// sender cache.
	if _, err := tx.Sender(); err != nil {
		return nil, err
	}
	return tx, nil
}

// bytesToUint64 reads a minimal big-endian RLP integer, rejecting an
// over-length value rather than silently truncating.
func bytesToUint64(b []byte) (uint64, error) {
	if len(b) > 8 {
		return 0, ErrEthField
	}
	var n uint64
	for _, c := range b {
		n = n<<8 | uint64(c)
	}
	return n, nil
}
