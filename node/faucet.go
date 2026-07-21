package node

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"sync"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/types"
)

// Faucet hands a new wallet enough LXS to pay gas for a first action (e.g.
// creating a token on the launchpad). A new wallet holds 0 LXS and cannot pay
// gas, so without this a new user is stuck at the first step. It runs on the
// node, funded by its own wallet that the operator tops up.
//
// The faucet dispenses real value (default 1 LXS), so anti-abuse is layered:
//  1. One claim per address (the claimed set below).
//  2. A strict per-IP rate limiter in front of this handler (wired in node.New);
//     without it a single client mints unlimited addresses, floods the mempool,
//     and drains the wallet as fast as it can POST.
//  3. The faucet wallet balance is the hard ceiling: it can only dispense what
//     the operator funded, and fails loud when empty.
//
// The claimed set is bounded (maxClaims) with oldest-first eviction: an unbounded
// map keyed by attacker-supplied addresses is itself a memory-exhaustion DoS. It
// resets on restart (abuse-slowing, not consensus state). Eviction can let a very
// old address claim twice, but only after maxClaims other claims pushed it out,
// each draining real balance.
const defaultMaxClaims = 200_000

type Faucet struct {
	key       *crypto.PrivateKey
	amount    *big.Int
	bc        *core.Blockchain
	pool      *mempool.Mempool
	broadcast func(*types.Transaction) error

	mu        sync.Mutex
	nextNonce uint64
	inited    bool
	claimed   map[common.Address]bool
	// ring is a fixed-size insertion log over claimed addresses. On wrap, the slot
	// about to be overwritten holds the oldest address and is evicted from
	// claimed, bounding memory at maxClaims entries.
	maxClaims int
	ring      []common.Address
	ringPos   int
	ringFull  bool
}

func NewFaucet(key *crypto.PrivateKey, amount *big.Int, bc *core.Blockchain, pool *mempool.Mempool, broadcast func(*types.Transaction) error) *Faucet {
	return &Faucet{key: key, amount: amount, bc: bc, pool: pool, broadcast: broadcast, claimed: map[common.Address]bool{}, maxClaims: defaultMaxClaims}
}

// remember records addr as claimed, evicting the oldest entry once the bounded set
// is full. Caller must hold f.mu.
func (f *Faucet) remember(addr common.Address) {
	if f.ring == nil {
		f.ring = make([]common.Address, f.maxClaims)
	}
	if f.ringFull {
		delete(f.claimed, f.ring[f.ringPos]) // evict the oldest
	}
	f.ring[f.ringPos] = addr
	f.ringPos++
	if f.ringPos == f.maxClaims {
		f.ringPos = 0
		f.ringFull = true
	}
	f.claimed[addr] = true
}

func (f *Faucet) Address() common.Address { return f.key.Address() }

// ServeHTTP answers POST {"address":"0x…"} by sending the fixed amount to that
// address. Permissive CORS is deliberate: the website (a different origin) calls
// it from the browser, and it is a public giveaway with no credentials.
func (f *Faucet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	addr, err := common.AddressFromHex(req.Address)
	if err != nil {
		http.Error(w, "bad address", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.claimed[addr] {
		http.Error(w, "this address already claimed from the faucet", http.StatusTooManyRequests)
		return
	}

	s := f.bc.StateSnapshot()
	// Keep the local nonce in sync with committed state: it starts at the account
	// nonce and bumps per dispense (the pool queues consecutive nonces); if state
	// has caught up, snap forward so a mined nonce is never re-used.
	if committed := s.Nonce(f.key.Address()); !f.inited || committed > f.nextNonce {
		f.nextNonce = committed
		f.inited = true
	}

	tx := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: f.bc.ChainID(), Nonce: f.nextNonce,
		To: &addr, Value: new(big.Int).Set(f.amount), GasLimit: 21000, GasPrice: big.NewInt(1),
	}
	if err := tx.Sign(f.key); err != nil {
		http.Error(w, "sign failed", http.StatusInternalServerError)
		return
	}
	// CheckState catches the failure that matters here: an empty faucet wallet.
	if err := mempool.CheckState(s, tx); err != nil {
		http.Error(w, "faucet unavailable (fund the faucet wallet): "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := f.pool.Add(tx, f.bc.ChainID()); err != nil {
		http.Error(w, "pool: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if f.broadcast != nil {
		_ = f.broadcast(tx)
	}
	f.nextNonce++
	f.remember(addr)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"tx": tx.Hash().Hex(), "amount": f.amount.String()})
}
