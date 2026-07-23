package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"lxs/common"
	"lxs/core"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

// The eth_ namespace is a compatibility facade that lets standard Ethereum
// tooling (MetaMask, ethers.js, Hardhat) talk to LXS as if it were go-ethereum.
// A thin layer over the chain_ methods: the same data under the names and shapes
// the ecosystem expects.
//
// Addresses line up for free: LXS derives them keccak256(pubkey)[12:],
// Ethereum's scheme, so a MetaMask account maps to the same address here.
func (a *API) registerEth(s *Server) {
	// Direct reuses: identical semantics under the Ethereum name. MetaMask sends
	// a trailing block tag ("latest") that decode ignores.
	s.Register("eth_chainId", a.ChainID)
	s.Register("eth_blockNumber", a.BlockNumber)
	s.Register("eth_getBalance", a.GetBalance)
	s.Register("eth_getTransactionCount", a.GetNonce)

	// New adapters.
	s.Register("net_version", a.NetVersion)
	s.Register("eth_gasPrice", a.GasPrice)
	s.Register("eth_getCode", a.GetCode)
	s.Register("eth_getStorageAt", a.GetStorageAt)
	s.Register("eth_call", a.EthCall)
	s.Register("eth_estimateGas", a.EthEstimateGas)
	s.Register("eth_getBlockByNumber", a.EthGetBlockByNumber)
	s.Register("eth_getBlockByHash", a.EthGetBlockByHash)

	// Write path: accept a MetaMask-signed tx and let the wallet track it.
	// MetaMask polls both getTransactionByHash (for the block number) and
	// getTransactionReceipt (for status); without the former a mined tx shows as
	// pending forever.
	s.Register("eth_sendRawTransaction", a.EthSendRawTransaction)
	s.Register("eth_getTransactionByHash", a.EthGetTransactionByHash)
	s.Register("eth_getTransactionReceipt", a.EthGetTransactionReceipt)
	s.Register("eth_getLogs", a.EthGetLogs)
}

// EthGetBlockByHash mirrors EthGetBlockByNumber, keyed by hash.
func (a *API) EthGetBlockByHash(params json.RawMessage) (interface{}, error) {
	var h common.Hash
	var full bool
	if err := decode(params, &h, &full); err != nil {
		return nil, err
	}
	blk, err := a.bc.BlockByHash(h)
	if err != nil {
		return nil, nil
	}
	return a.ethBlock(blk, full), nil
}

// EthGetTransactionByHash returns a transaction in Ethereum's shape. A non-null
// blockNumber is how MetaMask learns the tx was mined and flips it from pending
// to confirmed; a pending tx (still in the pool) returns null block fields.
func (a *API) EthGetTransactionByHash(params json.RawMessage) (interface{}, error) {
	var h common.Hash
	if err := decode(params, &h); err != nil {
		return nil, err
	}
	if tx, loc, err := a.bc.TxByHash(h); err == nil {
		return a.ethTxObject(tx, &loc), nil
	} else if !errors.Is(err, core.ErrUnknownTx) {
		return nil, err
	}
	if tx, ok := a.pool.Get(h); ok {
		return a.ethTxObject(tx, nil), nil // pending: block fields stay null
	}
	return nil, nil
}

func (a *API) ethTxObject(tx *types.Transaction, loc *core.TxLocation) map[string]interface{} {
	from, _ := tx.Sender()
	m := map[string]interface{}{
		"hash": tx.Hash(), "nonce": QU(tx.Nonce), "from": from, "to": tx.To,
		"value": Q(tx.Value), "gas": QU(tx.GasLimit), "gasPrice": Q(tx.GasPrice),
		"input": Data(tx.Data), "chainId": QU(tx.ChainID), "type": "0x0",
		"blockHash": nil, "blockNumber": nil, "transactionIndex": nil,
	}
	if loc != nil {
		m["blockHash"] = loc.BlockHash
		m["blockNumber"] = QU(loc.BlockHeight)
		m["transactionIndex"] = QU(loc.Index)
	}
	// The r/s/v a wallet expects to see for its own signed transaction.
	if tx.Type == types.TxTypeEthLegacy && len(tx.Sig) == 65 {
		recid := uint64(tx.Sig[0] - 27)
		m["v"] = QU(tx.ChainID*2 + 35 + recid)
		m["r"] = Data(tx.Sig[1:33])
		m["s"] = Data(tx.Sig[33:65])
	}
	return m
}

// EthSendRawTransaction accepts a raw EIP-155-signed transaction, recovers the
// sender against the EIP-155 signing hash, and admits it like a native tx (same
// nonce, balance, and pool checks). Returns the Ethereum tx hash so the wallet
// can track it.
func (a *API) EthSendRawTransaction(params json.RawMessage) (interface{}, error) {
	var raw Data
	if err := decode(params, &raw); err != nil {
		return nil, err
	}
	tx, err := types.ParseEthLegacyTx(raw)
	if err != nil {
		return nil, Err(CodeInvalidParams, "invalid raw transaction: "+err.Error())
	}
	// Same gates as any tx: stateful (nonce/balance) then, inside Add, stateless
	// (signature, low-s, chain id, gas) plus the pool bound.
	if err := mempool.CheckState(a.bc.StateSnapshot(), tx); err != nil {
		return nil, Err(CodeServerError, err.Error())
	}
	if err := a.pool.AddLocal(tx, a.bc.ChainID()); err != nil {
		return nil, Err(CodeServerError, err.Error())
	}
	if a.onTx != nil {
		_ = a.onTx(tx) // gossip the full Transaction, type included
	}
	return tx.Hash(), nil
}

// EthEstimateGas answers eth_estimateGas.
func (a *API) EthEstimateGas(params json.RawMessage) (interface{}, error) {
	var obj ethCallObject
	if err := decode(params, &obj); err != nil {
		return nil, err
	}
	from := common.Address{}
	if obj.From != nil {
		from = *obj.From
	}
	value := new(big.Int)
	if obj.Value != nil {
		value = obj.Value.Int
	}
	// obj.To == nil is a contract creation; pass it through so the estimate runs the
	// init code instead of mis-estimating a deploy at the flat intrinsic 21000.
	gas, err := a.bc.EstimateGas(from, obj.To, obj.Data, value)
	if err != nil {
		return nil, err // a revert surfaces as an error, like eth_call
	}
	return QU(gas), nil
}

// EthGetTransactionReceipt returns a receipt in Ethereum's shape, keyed by the
// Ethereum tx hash (for an eth-legacy tx, exactly tx.Hash()).
func (a *API) EthGetTransactionReceipt(params json.RawMessage) (interface{}, error) {
	var h common.Hash
	if err := decode(params, &h); err != nil {
		return nil, err
	}
	r, loc, err := a.bc.ReceiptByTxHash(h)
	if err != nil {
		return nil, nil // pending/unknown -> JSON null, the keep-waiting signal
	}
	tx, _, err := a.bc.TxByHash(h)
	if err != nil {
		return nil, nil
	}
	from, err := tx.Sender()
	if err != nil {
		return nil, err
	}

	// logIndex is the position within the whole BLOCK, not within this tx — the same
	// value eth_getLogs reports. Sum the logs of earlier txs in this block for the base.
	logBase := uint64(0)
	if receipts, _, err := a.bc.ReceiptsByHeight(loc.BlockHeight); err == nil {
		for j := 0; j < int(loc.Index) && j < len(receipts); j++ {
			logBase += uint64(len(receipts[j].Logs))
		}
	}
	logs := make([]interface{}, 0, len(r.Logs))
	for i, l := range r.Logs {
		logs = append(logs, map[string]interface{}{
			"address": l.Address, "topics": l.Topics, "data": Data(l.Data),
			"blockNumber": QU(loc.BlockHeight), "blockHash": loc.BlockHash,
			"transactionHash": h, "transactionIndex": QU(loc.Index),
			"logIndex": QU(logBase + uint64(i)), "removed": false,
		})
	}
	out := map[string]interface{}{
		"transactionHash":   h,
		"transactionIndex":  QU(loc.Index),
		"blockHash":         loc.BlockHash,
		"blockNumber":       QU(loc.BlockHeight),
		"from":              from,
		"to":                tx.To,
		"cumulativeGasUsed": QU(r.CumulativeGasUsed),
		"gasUsed":           QU(r.GasUsed),
		"effectiveGasPrice": "0x" + tx.GasPrice.Text(16),
		"status":            QU(r.Status),
		"logs":              logs,
		"logsBloom":         "0x" + strings.Repeat("00", 256),
		"contractAddress":   nil,
		"type":              "0x0",
	}
	if tx.To == nil {
		addr := state.CreateAddress(from, tx.Nonce)
		out["contractAddress"] = addr
	}
	return out, nil
}

// NetVersion is the legacy network id as a decimal string (unlike eth_chainId's
// hex). MetaMask asks for both and requires they agree.
func (a *API) NetVersion(json.RawMessage) (interface{}, error) {
	return strconv.FormatUint(a.bc.ChainID(), 10), nil
}

// GasPrice reports the price the chain expects (a flat 1 on devnet). A wallet
// needs some value to multiply the gas limit by when building a transaction.
func (a *API) GasPrice(json.RawMessage) (interface{}, error) {
	return QU(1), nil
}

// GetCode returns a contract's runtime bytecode (empty for a plain account):
// how a wallet or explorer tells a contract from an account.
func (a *API) GetCode(params json.RawMessage) (interface{}, error) {
	var addr common.Address
	if err := decode(params, &addr); err != nil {
		return nil, err
	}
	return Data(a.bc.CodeAt(addr)), nil
}

// GetStorageAt reads a single 32-byte storage slot of a contract.
func (a *API) GetStorageAt(params json.RawMessage) (interface{}, error) {
	var addr common.Address
	var slot common.Hash
	if err := decode(params, &addr, &slot); err != nil {
		return nil, err
	}
	v := a.bc.StorageAt(addr, slot)
	return Data(v[:]), nil
}

// ethCallObject is the call object MetaMask/ethers send as the first parameter
// of eth_call: a subset of a transaction, all hex.
type ethCallObject struct {
	From  *common.Address `json:"from"`
	To    *common.Address `json:"to"`
	Gas   *Quantity       `json:"gas"`
	Value *Quantity       `json:"value"`
	Data  Data            `json:"data"`
}

// EthCall is a read-only contract execution: it maps the Ethereum call object
// onto chain.Call and returns the raw output. Lets a dapp read
// balanceOf/name/symbol over the standard interface.
func (a *API) EthCall(params json.RawMessage) (interface{}, error) {
	var obj ethCallObject
	if err := decode(params, &obj); err != nil {
		return nil, err
	}
	if obj.To == nil {
		return nil, Err(CodeInvalidParams, "eth_call requires a 'to' address")
	}
	from := common.Address{}
	if obj.From != nil {
		from = *obj.From
	}
	ret, err := a.bc.Call(from, *obj.To, obj.Data, cappedCallGas(obj.Gas))
	if err != nil {
		return nil, Err(CodeServerError, "execution reverted: "+err.Error())
	}
	return Data(ret), nil
}

// EthGetBlockByNumber resolves a block tag ("latest"/"earliest"/hex) and returns
// the block in Ethereum's shape. MetaMask polls "latest" to track the chain.
func (a *API) EthGetBlockByNumber(params json.RawMessage) (interface{}, error) {
	var tag string
	var full bool
	if err := decode(params, &tag, &full); err != nil {
		return nil, err
	}
	var height uint64
	switch tag {
	case "latest", "pending", "safe", "finalized":
		height = a.bc.Head().Height()
	case "earliest":
		height = 0
	default:
		h, err := strconv.ParseUint(strings.TrimPrefix(tag, "0x"), 16, 64)
		if err != nil {
			return nil, Err(CodeInvalidParams, "bad block tag: "+tag)
		}
		height = h
	}
	blk, err := a.bc.BlockByHeight(height)
	if err != nil {
		return nil, nil // JSON null: no such block
	}
	return a.ethBlock(blk, full), nil
}

// ethBlock renders a block in Ethereum's JSON shape. Fields LXS lacks (bloom,
// uncles) are zero-filled, not omitted: some clients reject a block object with
// missing keys.
func (a *API) ethBlock(blk *types.Block, full bool) map[string]interface{} {
	bh := blk.Hash()
	txs := make([]interface{}, 0, len(blk.Txs))
	for i, tx := range blk.Txs {
		if full {
			from, _ := tx.Sender()
			txs = append(txs, map[string]interface{}{
				"hash": tx.Hash(), "from": from, "to": tx.To,
				"nonce": QU(tx.Nonce), "value": Q(tx.Value),
				"gas": QU(tx.GasLimit), "gasPrice": Q(tx.GasPrice), "input": Data(tx.Data),
				"blockHash": bh, "blockNumber": QU(blk.Header.Height),
				"transactionIndex": QU(uint64(i)), "chainId": QU(tx.ChainID), "type": "0x0",
			})
		} else {
			txs = append(txs, tx.Hash())
		}
	}
	return map[string]interface{}{
		"number":           QU(blk.Header.Height),
		"hash":             blk.Hash(),
		"parentHash":       blk.Header.ParentHash,
		"timestamp":        QU(uint64(blk.Header.Timestamp) / 1000), // ms -> s, Ethereum's unit
		"gasLimit":         QU(blk.Header.GasLimit),
		"gasUsed":          QU(blk.Header.GasUsed),
		"miner":            blk.Header.Proposer,
		"difficulty":       QU(blk.Header.Difficulty),
		"totalDifficulty":  QU(blk.Header.Difficulty),
		"stateRoot":        blk.Header.StateRoot,
		"transactionsRoot": blk.Header.TxRoot,
		"receiptsRoot":     blk.Header.ReceiptRoot,
		"transactions":     txs,
		"nonce":            fmt.Sprintf("0x%016x", blk.Header.Nonce),
		"size":             QU(0),
		"extraData":        "0x",
		"logsBloom":        "0x" + strings.Repeat("00", 256),
		"sha3Uncles":       common.Hash{},
		"uncles":           []interface{}{},
	}
}
