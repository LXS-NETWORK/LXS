package mempool

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"

	"lxs/common"
	"lxs/state"
	"lxs/types"
)

var (
	ErrAlreadyKnown = errors.New("mempool: transaction already known")
	ErrPoolFull     = errors.New("mempool: pool is full")
	ErrNonceUsed    = errors.New("mempool: nonce already used")
	ErrNonceStale   = errors.New("mempool: nonce below account state")
	ErrCannotPay    = errors.New("mempool: balance cannot cover cost")
	ErrUnderpriced  = errors.New("mempool: gas price below the node's admission floor")
	ErrAccountLimit = errors.New("mempool: too many queued transactions for one account")
)

// maxPerAccount caps queued txs from a single sender (geth's AccountQueue default),
// so no one account can monopolise the pool.
const maxPerAccount = 64

// CheckState is the stateful admission gate that Add (deliberately stateless)
// cannot do. It judges a gossiped tx against committed head state before it takes
// a mempool slot; the mempool is the most exposed surface and its slots are what
// an attacker wants to exhaust for free.
//
// Two checks against committed head state:
//
//   - Nonce not stale. A nonce below the account's is already spent and can never
//     execute. A nonce above is fine: a queued tx that Pending() sequences later.
//   - Balance covers cost. gasLimit*gasPrice + value must be affordable at head,
//     the spam floor against floods from empty accounts that will never pay.
//
// Admission, not consensus: a tx that passes here can still fail when executed in
// a block (an earlier tx drains the balance). The block is the authority.
func CheckState(s *state.State, tx *types.Transaction) error {
	sender, err := tx.Sender()
	if err != nil {
		return err
	}
	if tx.Nonce < s.Nonce(sender) {
		return fmt.Errorf("%w: tx nonce %d, account at %d", ErrNonceStale, tx.Nonce, s.Nonce(sender))
	}
	if s.Balance(sender).Cmp(tx.Cost()) < 0 {
		return fmt.Errorf("%w: cost %s, balance %s", ErrCannotPay, tx.Cost(), s.Balance(sender))
	}
	return nil
}

// Mempool holds pending transactions. It is the most exposed part of a node:
// anyone on the network can push into it for free. So it must be bounded (maxSize)
// and accepting a tx must be cheap and never a promise to include. A mempool is a
// hint, not a queue; nothing in consensus depends on it.
type Mempool struct {
	mu      sync.RWMutex
	maxSize int
	// minGasPrice is an admission policy, not a consensus rule: this node refuses txs
	// priced below it, but a block containing a cheaper tx is still valid (else nodes
	// with different floors diverge). Blunts gasPrice-0 spam. nil/zero = no floor.
	minGasPrice *big.Int
	all         map[common.Hash]*types.Transaction
	byAcct      map[common.Address]map[uint64]*types.Transaction
}

func New(maxSize int) *Mempool {
	return &Mempool{
		maxSize: maxSize,
		all:     make(map[common.Hash]*types.Transaction),
		byAcct:  make(map[common.Address]map[uint64]*types.Transaction),
	}
}

// SetMinGasPrice sets the admission floor (nil/zero = no floor). Off by default so
// the mempool stays consensus-neutral unless an operator opts in.
func (m *Mempool) SetMinGasPrice(p *big.Int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.minGasPrice = p
}

func (m *Mempool) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.all)
}

// Add performs stateless validation and inserts the tx, applying the admission floor.
func (m *Mempool) Add(tx *types.Transaction, chainID uint64) error {
	return m.add(tx, chainID, true)
}

// add is the shared insert. enforceFloor is false only for Reinject: a tx already
// mined is consensus-acceptable, so re-pooling it after a reorg must not drop it
// for being below this node's current admission floor.
func (m *Mempool) add(tx *types.Transaction, chainID uint64, enforceFloor bool) error {
	if err := tx.SanityCheck(chainID); err != nil {
		return err
	}
	sender, err := tx.Sender()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Admission floor (policy, not consensus): reject underpriced spam before it takes a slot.
	if enforceFloor && m.minGasPrice != nil && m.minGasPrice.Sign() > 0 && tx.GasPrice.Cmp(m.minGasPrice) < 0 {
		return ErrUnderpriced
	}
	if _, ok := m.all[tx.Hash()]; ok {
		return ErrAlreadyKnown
	}
	if m.byAcct[sender] == nil {
		m.byAcct[sender] = make(map[uint64]*types.Transaction)
	}
	if _, ok := m.byAcct[sender][tx.Nonce]; ok {
		// Replace-by-fee (accept a higher gas price) is deliberately omitted: RBF
		// has subtle griefing dynamics.
		return ErrNonceUsed
	}
	// Per-account cap. Without it one sender fills the whole pool with a burst of
	// future-nonce txs — and since each is checked against head balance independently,
	// a zero-balance, zero-fee account can queue thousands of unexecutable txs for
	// free, starving honest txs and freezing block production. geth caps queued txs
	// per account for exactly this.
	if len(m.byAcct[sender]) >= maxPerAccount {
		return ErrAccountLimit
	}
	// Full pool: evict the lowest-priced tx if the newcomer outbids it, so a high-fee
	// tx is never shut out by a low-fee backlog. Reject only if it cannot outbid the
	// cheapest resident.
	if len(m.all) >= m.maxSize {
		cheapest := m.lowestPricedLocked()
		if cheapest == nil || tx.GasPrice.Cmp(cheapest.GasPrice) <= 0 {
			return ErrPoolFull
		}
		// Evict the TAIL (highest nonce) of the cheapest resident's account, not the cheapest
		// tx itself: dropping a low/middle nonce opens a gap that strands that account's later
		// txs (Pending stops at the gap). The tail is always safe to drop. The newcomer still
		// only enters by outbidding the cheapest resident.
		victimSender, err := cheapest.Sender()
		if err != nil {
			return ErrPoolFull
		}
		var tail *types.Transaction
		for _, t := range m.byAcct[victimSender] {
			if tail == nil || t.Nonce > tail.Nonce {
				tail = t
			}
		}
		m.dropLocked(tail)
	}
	m.all[tx.Hash()] = tx
	m.byAcct[sender][tx.Nonce] = tx
	return nil
}

// lowestPricedLocked returns the pool's cheapest tx (held with the lock). O(n), fine
// at our pool size; a priced heap would be the move at much larger scale.
func (m *Mempool) lowestPricedLocked() *types.Transaction {
	var min *types.Transaction
	for _, tx := range m.all {
		if min == nil || tx.GasPrice.Cmp(min.GasPrice) < 0 {
			min = tx
		}
	}
	return min
}

// dropLocked removes one tx from both indexes (held with the lock).
func (m *Mempool) dropLocked(tx *types.Transaction) {
	delete(m.all, tx.Hash())
	if sender, err := tx.Sender(); err == nil {
		if byNonce, ok := m.byAcct[sender]; ok {
			delete(byNonce, tx.Nonce)
			if len(byNonce) == 0 {
				delete(m.byAcct, sender)
			}
		}
	}
}

func (m *Mempool) Get(h common.Hash) (*types.Transaction, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tx, ok := m.all[h]
	return tx, ok
}

// Pending selects an executable, correctly ordered batch for a block. A tx with
// nonce 7 is unexecutable if the account is at 5 and nobody sent 6, so each
// account's nonces are walked from its state value and stopped at the first gap.
// Accounts are ordered by gas price; within an account, order is forced by nonce.
func (m *Mempool) Pending(s *state.State, gasLimit uint64) []*types.Transaction {
	m.mu.RLock()
	defer m.mu.RUnlock()

	type candidate struct {
		txs []*types.Transaction
	}
	var groups []candidate

	for sender, byNonce := range m.byAcct {
		next := s.Nonce(sender)
		var run []*types.Transaction
		for {
			tx, ok := byNonce[next]
			if !ok {
				break // gap: everything after this is unexecutable
			}
			run = append(run, tx)
			next++
		}
		if len(run) > 0 {
			groups = append(groups, candidate{txs: run})
		}
	}

	// Sort accounts by the gas price of their first executable tx.
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].txs[0].GasPrice.Cmp(groups[j].txs[0].GasPrice) > 0
	})

	// Size each tx by its OWN gas limit, not a flat intrinsic: a tx can consume up
	// to its limit, so summing limits keeps the block's actual gas under gasLimit. A
	// flat-intrinsic count undersizes data-carrying and contract txs, and the producer
	// would then build a block its own ApplyBlock rejects (ErrGasLimit). A plain
	// transfer's limit is the intrinsic, so common-case packing is unchanged.
	var out []*types.Transaction
	var gas uint64
	for _, g := range groups {
		for _, tx := range g.txs {
			if gas+tx.GasLimit > gasLimit {
				return out
			}
			gas += tx.GasLimit
			out = append(out, tx)
		}
	}
	return out
}

// Demote drops txs that can no longer execute: any whose nonce is below the sender's
// committed state nonce. These arise when a competing tx with the same (sender,nonce)
// was mined (Remove deletes only the exact mined hash) or after a reorg. Pending skips
// them, so they are invisible dead weight occupying slots; a periodic Demote against
// the current head keeps the pool from silently filling with un-minable txs. Returns
// how many were dropped.
func (m *Mempool) Demote(s *state.State) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	dropped := 0
	for sender, byNonce := range m.byAcct {
		committed := s.Nonce(sender)
		for nonce, tx := range byNonce {
			if nonce < committed {
				delete(m.all, tx.Hash())
				delete(byNonce, nonce)
				dropped++
			}
		}
		if len(byNonce) == 0 {
			delete(m.byAcct, sender)
		}
	}
	return dropped
}

// PendingNonce returns the nonce a new tx from addr should use: the account's
// committed nonce (base) advanced past every consecutive queued tx. A wallet firing
// several txs quickly asks for this via eth_getTransactionCount(addr,"pending") so it
// does not reuse a nonce already sitting in the pool.
func (m *Mempool) PendingNonce(addr common.Address, base uint64) uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byNonce, ok := m.byAcct[addr]
	if !ok {
		return base
	}
	next := base
	for {
		if _, ok := byNonce[next]; !ok {
			return next
		}
		next++
	}
}

// Remove drops transactions that made it into a block.
func (m *Mempool) Remove(txs []*types.Transaction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tx := range txs {
		delete(m.all, tx.Hash())
		if sender, err := tx.Sender(); err == nil {
			if byNonce, ok := m.byAcct[sender]; ok {
				delete(byNonce, tx.Nonce)
				if len(byNonce) == 0 {
					delete(m.byAcct, sender)
				}
			}
		}
	}
}

// Reinject takes back transactions from blocks orphaned by a reorg. Without it a
// reorg silently drops them: they left the pool when mined, and the block that
// mined them no longer exists, so the sender's money never moves despite a
// pre-reorg receipt.
//
// Conflicts are tolerated: if a tx for the same (sender, nonce) is already
// pending, the pending one wins and the re-injected one is dropped. What matters
// is not holding two txs that can never both execute.
//
// Errors are swallowed: a re-injected tx that no longer fits (already re-mined,
// nonce overtaken, pool full) is a reorg doing what reorgs do, not a fault.
func (m *Mempool) Reinject(txs []*types.Transaction, chainID uint64) (accepted int) {
	for _, tx := range txs {
		// enforceFloor=false: these txs were mined, so they passed consensus; the
		// admission floor must not lose them on a reorg.
		if err := m.add(tx, chainID, false); err == nil {
			accepted++
		}
	}
	return accepted
}
