package rpc

import (
	"encoding/json"
	"errors"

	"lxs/common"
	"lxs/core"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

// callGasCap bounds a read-only eth_call. A call charges nobody, so it needs its
// own ceiling; without one an infinite loop in a view function spins the node
// forever for free.
const callGasCap uint64 = 50_000_000

// cappedCallGas returns min(request, callGasCap), or the cap when no request is
// given. A caller may ask for less but never more: an unbounded request is a
// free-compute DoS.
func cappedCallGas(req *Quantity) uint64 {
	if req == nil {
		return callGasCap
	}
	if g := req.U64(); g < callGasCap {
		return g
	}
	return callGasCap
}

// API wires the node's internals to RPC methods. It holds no key material and
// cannot sign: signing is client-side, in the wallet. Node-side account
// unlocking (Ethereum's personal_unlockAccount) plus an exposed port has
// drained thousands of nodes.
type API struct {
	bc   *core.Blockchain
	pool *mempool.Mempool

	// onTx, if set, gossips a locally submitted tx. A hook, not a p2p import:
	// rpc must not depend on p2p. nil on a node with no networking; the tx
	// still enters the local pool.
	onTx func(*types.Transaction) error
}

func NewAPI(bc *core.Blockchain, pool *mempool.Mempool) *API {
	return &API{bc: bc, pool: pool}
}

// SetTxBroadcaster wires the gossip hook, fired after a locally submitted tx is
// admitted to the pool.
func (a *API) SetTxBroadcaster(fn func(*types.Transaction) error) { a.onTx = fn }

func (a *API) Register(s *Server) {
	s.Register("chain_chainId", a.ChainID)
	s.Register("chain_blockNumber", a.BlockNumber)
	s.Register("chain_getBalance", a.GetBalance)
	s.Register("chain_getNonce", a.GetNonce)
	s.Register("chain_getBlockByNumber", a.GetBlockByNumber)
	s.Register("chain_getBlockByHash", a.GetBlockByHash)
	s.Register("chain_sendTransaction", a.SendTransaction)
	s.Register("chain_getTransactionByHash", a.GetTransactionByHash)
	s.Register("chain_getTransactionReceipt", a.GetTransactionReceipt)
	s.Register("chain_call", a.Call)
	s.Register("txpool_status", a.TxPoolStatus)

	// Ethereum-compatibility facade (eth_*, net_*) for MetaMask and ethers.
	a.registerEth(s)
}

// Call runs a read-only contract call: execute `to`'s code against current
// state and return its output, committing nothing. Reads a token balance
// without sending a transaction.
func (a *API) Call(params json.RawMessage) (interface{}, error) {
	var args CallArgs
	if err := decode(params, &args); err != nil {
		return nil, err
	}
	from := common.Address{}
	if args.From != nil {
		from = *args.From
	}
	ret, err := a.bc.Call(from, args.To, args.Data, cappedCallGas(args.Gas))
	if err != nil {
		// A revert is a real answer, not a server fault; surface it with any
		// reason bytes.
		return nil, Err(CodeServerError, "call reverted: "+err.Error())
	}
	return Data(ret), nil
}

func decode(params json.RawMessage, out ...interface{}) error {
	var raw []json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &raw); err != nil {
			return Err(CodeInvalidParams, "params must be an array")
		}
	}
	if len(raw) < len(out) {
		return Err(CodeInvalidParams, "not enough parameters")
	}
	for i, target := range out {
		if err := json.Unmarshal(raw[i], target); err != nil {
			return Err(CodeInvalidParams, err.Error())
		}
	}
	return nil
}

func (a *API) ChainID(json.RawMessage) (interface{}, error) {
	return QU(a.bc.ChainID()), nil
}

func (a *API) BlockNumber(json.RawMessage) (interface{}, error) {
	return QU(a.bc.Head().Height()), nil
}

func (a *API) GetBalance(params json.RawMessage) (interface{}, error) {
	var addr common.Address
	if err := decode(params, &addr); err != nil {
		return nil, err
	}
	return Q(a.bc.BalanceAt(addr)), nil
}

// GetNonce returns the account's next expected nonce. Reads committed state
// only, not the mempool: a wallet firing several txs quickly must track its own
// pending nonces, else two wallets on one key silently collide.
func (a *API) GetNonce(params json.RawMessage) (interface{}, error) {
	var raw []json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &raw); err != nil {
			return nil, Err(CodeInvalidParams, "params must be an array")
		}
	}
	if len(raw) < 1 {
		return nil, Err(CodeInvalidParams, "not enough parameters")
	}
	var addr common.Address
	if err := json.Unmarshal(raw[0], &addr); err != nil {
		return nil, Err(CodeInvalidParams, err.Error())
	}
	committed := a.bc.NonceAt(addr)
	// The "pending" tag must include txs already queued locally, or a wallet firing
	// several txs in a row reuses a nonce and every tx after the first is rejected.
	if len(raw) > 1 {
		var tag string
		if json.Unmarshal(raw[1], &tag) == nil && tag == "pending" {
			return QU(a.pool.PendingNonce(addr, committed)), nil
		}
	}
	return QU(committed), nil
}

func (a *API) GetBlockByNumber(params json.RawMessage) (interface{}, error) {
	var height Quantity
	var full bool
	if err := decode(params, &height, &full); err != nil {
		return nil, err
	}
	blk, err := a.bc.BlockByHeight(height.U64())
	if err != nil {
		return nil, nil // JSON null: no such block is not an error
	}
	return a.blockResult(blk, full)
}

func (a *API) GetBlockByHash(params json.RawMessage) (interface{}, error) {
	var h common.Hash
	var full bool
	if err := decode(params, &h, &full); err != nil {
		return nil, err
	}
	blk, err := a.bc.BlockByHash(h)
	if err != nil {
		return nil, nil
	}
	return a.blockResult(blk, full)
}

func (a *API) blockResult(blk *types.Block, full bool) (interface{}, error) {
	res := &BlockResult{
		HeaderResult: HeaderResult{
			Hash:        blk.Hash(),
			ParentHash:  blk.Header.ParentHash,
			Height:      QU(blk.Header.Height),
			Timestamp:   QU(uint64(blk.Header.Timestamp)),
			TxRoot:      blk.Header.TxRoot,
			ReceiptRoot: blk.Header.ReceiptRoot,
			StateRoot:   blk.Header.StateRoot,
			GasUsed:     QU(blk.Header.GasUsed),
			GasLimit:    QU(blk.Header.GasLimit),
			Proposer:    blk.Header.Proposer,
		},
		Txs: make([]interface{}, 0, len(blk.Txs)),
	}
	for i, tx := range blk.Txs {
		if !full {
			res.Txs = append(res.Txs, tx.Hash())
			continue
		}
		tr, err := a.txResult(tx, &core.TxLocation{
			BlockHash: blk.Hash(), BlockHeight: blk.Height(), Index: uint64(i),
		})
		if err != nil {
			return nil, err
		}
		res.Txs = append(res.Txs, tr)
	}
	return res, nil
}

func (a *API) txResult(tx *types.Transaction, loc *core.TxLocation) (*TxResult, error) {
	from, err := tx.Sender()
	if err != nil {
		return nil, Err(CodeServerError, "cannot recover sender: "+err.Error())
	}
	out := &TxResult{
		Hash:     tx.Hash(),
		From:     from,
		To:       tx.To,
		Nonce:    QU(tx.Nonce),
		Value:    Q(tx.Value),
		GasLimit: QU(tx.GasLimit),
		GasPrice: Q(tx.GasPrice),
		Data:     tx.Data,
	}
	if loc != nil {
		h, height, idx := loc.BlockHash, QU(loc.BlockHeight), QU(loc.Index)
		out.BlockHash, out.BlockHeight, out.TxIndex = &h, &height, &idx
	}
	return out, nil
}

// SendTransaction accepts an already-signed transaction (the name follows
// Ethereum convention; nothing is signed here). The node validates and gossips,
// never holding a key.
func (a *API) SendTransaction(params json.RawMessage) (interface{}, error) {
	var args TxArgs
	if err := decode(params, &args); err != nil {
		return nil, err
	}
	tx := args.ToTx()

	// Same gates as a tx off the wire: stateful (nonce, balance) then, inside
	// Add, stateless (sig, type, low-s, chain id, gas) plus the pool bound.
	if err := mempool.CheckState(a.bc.StateSnapshot(), tx); err != nil {
		return nil, Err(CodeServerError, err.Error())
	}
	if err := a.pool.Add(tx, a.bc.ChainID()); err != nil {
		// Rejection reasons are safe to return: the client needs them to bump
		// the nonce, raise the fee, or give up.
		return nil, Err(CodeServerError, err.Error())
	}

	// Announce only after the tx is in the pool. A broadcast failure is not a
	// submission failure: the tx is admitted, so it is logged by the hook, not
	// surfaced to the client.
	if a.onTx != nil {
		_ = a.onTx(tx)
	}
	return tx.Hash(), nil
}

func (a *API) GetTransactionByHash(params json.RawMessage) (interface{}, error) {
	var h common.Hash
	if err := decode(params, &h); err != nil {
		return nil, err
	}
	if tx, loc, err := a.bc.TxByHash(h); err == nil {
		return a.txResult(tx, &loc)
	} else if !errors.Is(err, core.ErrUnknownTx) {
		return nil, err
	}
	// Not mined but possibly pending. Null block fields are how a caller tells
	// pending from mined.
	if tx, ok := a.pool.Get(h); ok {
		return a.txResult(tx, nil)
	}
	return nil, nil
}

func (a *API) GetTransactionReceipt(params json.RawMessage) (interface{}, error) {
	var h common.Hash
	if err := decode(params, &h); err != nil {
		return nil, err
	}
	r, loc, err := a.bc.ReceiptByTxHash(h)
	if err != nil {
		// A pending tx has no receipt and must not get a fake one; null tells
		// the client to keep waiting.
		return nil, nil
	}
	tx, _, err := a.bc.TxByHash(h)
	if err != nil {
		return nil, err
	}
	from, err := tx.Sender()
	if err != nil {
		return nil, err
	}
	res := &ReceiptResult{
		TxHash:            h,
		Status:            QU(r.Status),
		GasUsed:           QU(r.GasUsed),
		CumulativeGasUsed: QU(r.CumulativeGasUsed),
		BlockHash:         loc.BlockHash,
		BlockHeight:       QU(loc.BlockHeight),
		TxIndex:           QU(loc.Index),
		From:              from,
		To:                tx.To,
		Logs:              make([]LogResult, 0, len(r.Logs)),
	}
	// A deployment's receipt carries the created contract address, derived from
	// the sender and the nonce it used.
	if tx.To == nil {
		addr := state.CreateAddress(from, tx.Nonce)
		res.ContractAddress = &addr
	}
	for _, l := range r.Logs {
		res.Logs = append(res.Logs, LogResult{Address: l.Address, Topics: l.Topics, Data: l.Data})
	}
	return res, nil
}

func (a *API) TxPoolStatus(json.RawMessage) (interface{}, error) {
	return map[string]interface{}{"pending": QU(uint64(a.pool.Len()))}, nil
}
