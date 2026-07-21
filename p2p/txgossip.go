package p2p

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"lxs/core"
	"lxs/mempool"
	"lxs/types"
)

var (
	ErrTxMessageTooLarge = errors.New("p2p: tx message too large")
	ErrBadTxEncoding     = errors.New("p2p: malformed tx message")
)

// maxTxMessage caps one inbound tx message. Same rationale as maxBlockMessage:
// json.Unmarshal on an unbounded slice is a peer's cheapest way to force
// allocation, and the mempool is the surface they most want to flood. One tx per
// message keeps validation and rejection at the same granularity.
const maxTxMessage = 128 << 10 // 128 KiB

// txMessage is the wire format: one transaction, JSON, re-derived and
// revalidated on receipt. Nothing in it is trusted to be canonical.
type txMessage struct {
	Tx *types.Transaction `json:"tx"`
}

// TxGossip propagates transactions and guards the mempool. Same shape as block
// Gossip: delivery duplicates and reorders, the cheap check comes first, hostile
// input is assumed. The difference is the threat: a bad block wastes a
// validation, a bad tx wastes a finite mempool slot. So every admitted tx clears
// two gates — stateless (signature, type, low-s, chain id, gas) and stateful
// (nonce not spent, balance covers cost) — before it takes a slot.
type TxGossip struct {
	net     Network
	bc      *core.Blockchain
	pool    *mempool.Mempool
	log     *log.Logger
	scorer  *Scorer
	limiter *txRateLimiter // per-peer inbound-tx flood control

	mu    sync.Mutex
	Stats TxStats
}

type TxStats struct {
	Received int
	// Duplicate: already held. On a healthy mesh most inbound txs are duplicates
	// (every peer forwards), so a large count is normal and cheap.
	Duplicate int
	Accepted  int
	// Rejected: failed a validation gate, which scores a peer toward a ban.
	// Dropped: ignored because the sender is already banned.
	Rejected int
	Dropped  int
}

type TxOption func(*TxGossip)

func WithTxLogger(l *log.Logger) TxOption { return func(t *TxGossip) { t.log = l } }

// WithTxScorer shares a peer scorer with tx gossip: a peer whose transactions
// keep failing validation is penalised and eventually dropped.
func WithTxScorer(s *Scorer) TxOption { return func(t *TxGossip) { t.scorer = s } }

func NewTxGossip(net Network, bc *core.Blockchain, pool *mempool.Mempool, opts ...TxOption) (*TxGossip, error) {
	tg := &TxGossip{net: net, bc: bc, pool: pool}
	for _, o := range opts {
		o(tg)
	}
	if tg.limiter == nil {
		tg.limiter = newTxRateLimiter(0, 0, nil) // sane defaults
	}
	if err := net.Subscribe(TopicTxs, tg.onTx); err != nil {
		return nil, err
	}
	return tg, nil
}

// Broadcast publishes a locally accepted transaction (from RPC). Like a produced
// block, only after it is in the local pool: announcing a tx we then reject would
// advertise something we do not hold.
func (tg *TxGossip) Broadcast(tx *types.Transaction) error {
	data, err := json.Marshal(txMessage{Tx: tx})
	if err != nil {
		return err
	}
	return tg.net.Publish(TopicTxs, data)
}

// Accept validates a transaction and, if good and new, admits it to the pool.
// The single admission gate: RPC calls it for a local tx, onTx for a gossiped
// one, so both clear the same checks. Returns nil if the tx is now (or already
// was) in the pool, an error if refused.
func (tg *TxGossip) Accept(tx *types.Transaction) error {
	if tx == nil {
		return ErrBadTxEncoding
	}
	// Cheap dedup first, before recovering a signature or copying state.
	if _, known := tg.pool.Get(tx.Hash()); known {
		return mempool.ErrAlreadyKnown
	}
	// Stateless gate before stateful: a malformed tx (bad type/chainid/gas/sig)
	// must surface its form error, which penalises the relayer, rather than be
	// masked by an ErrCannotPay from a zero-balance signer, which does not.
	// Order matters for scoring.
	if err := tx.SanityCheck(tg.bc.ChainID()); err != nil {
		return err
	}
	// Stateful gate against committed head state: nonce not spent, can pay.
	if err := mempool.CheckState(tg.bc.StateSnapshot(), tx); err != nil {
		return err
	}
	// Add re-runs the stateless gate and enforces the pool bound and per-account
	// nonce uniqueness.
	return tg.pool.Add(tx, tg.bc.ChainID())
}

// onTx handles an inbound transaction. Hostile input is the default assumption.
func (tg *TxGossip) onTx(from PeerID, data []byte) error {
	// A banned peer's traffic is dropped before it costs anything to decode.
	if tg.scorer != nil && tg.scorer.Banned(from) {
		tg.bump(&tg.Stats.Dropped)
		return nil
	}
	// Flood control: a peer over its inbound-tx rate has its excess dropped
	// before the decode + EC-recover, without penalty (an honest relayer at
	// normal rate never trips it). Backstop for the valid-sig-but-unfundable
	// flood the Scorer must not ban.
	if tg.limiter != nil && !tg.limiter.allow(from) {
		tg.bump(&tg.Stats.Dropped)
		return nil
	}

	tg.bump(&tg.Stats.Received)

	if len(data) > maxTxMessage {
		tg.penalize(from)
		return fmt.Errorf("%w: %d bytes from %s", ErrTxMessageTooLarge, len(data), from)
	}

	var msg txMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg.Tx == nil {
		tg.penalize(from)
		return fmt.Errorf("%w from %s", ErrBadTxEncoding, from)
	}

	switch err := tg.Accept(msg.Tx); {
	case err == nil:
		tg.bump(&tg.Stats.Accepted)
		return nil
	case errors.Is(err, mempool.ErrAlreadyKnown):
		tg.bump(&tg.Stats.Duplicate)
		return nil
	case isTransientReject(err):
		// Not acceptable right now: pool full, nonce just mined, or sender can't
		// pay yet. None is the relayer's fault; they are normal propagation
		// races. Penalising them would, through the non-decaying Scorer, slowly
		// ban honest relayers and partition the mesh. Drop without penalty
		// (mirrors block gossip separating ErrKnownBlock/ErrFutureBlock/
		// ErrUnknownParent from a genuinely-invalid block).
		tg.bump(&tg.Stats.Dropped)
		return fmt.Errorf("p2p: dropped tx %s from %s (transient): %w", msg.Tx.Hash().Hex(), from, err)
	default:
		// Stateless-invalid: bad signature, wrong chain id, bad type/value/gas.
		// The tx is forged or corrupt, which only the sender can cause. Penalise.
		tg.penalize(from)
		return fmt.Errorf("p2p: rejected invalid tx %s from %s: %w", msg.Tx.Hash().Hex(), from, err)
	}
}

// isTransientReject reports whether a rejection is a normal propagation race
// (not the relaying peer's fault) rather than proof the sender forged the tx.
func isTransientReject(err error) bool {
	return errors.Is(err, mempool.ErrPoolFull) ||
		errors.Is(err, mempool.ErrNonceUsed) ||
		errors.Is(err, mempool.ErrNonceStale) ||
		errors.Is(err, mempool.ErrCannotPay) ||
		errors.Is(err, mempool.ErrUnderpriced) // sender's low fee / our local floor, not forgery
}

// penalize records a rejected tx and scores its sender toward a ban.
func (tg *TxGossip) penalize(from PeerID) {
	tg.bump(&tg.Stats.Rejected)
	if tg.scorer != nil {
		if tg.scorer.Penalize(from, 1) {
			if tg.log != nil {
				tg.log.Printf("p2p: banned peer %s (too many bad txs)", from)
			}
		}
	}
}

func (tg *TxGossip) bump(counter *int) {
	tg.mu.Lock()
	*counter++
	tg.mu.Unlock()
}

// Snapshot returns a copy of the counters.
func (tg *TxGossip) Snapshot() TxStats {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.Stats
}
