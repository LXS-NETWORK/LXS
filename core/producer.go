package core

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"lxs/common"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

// applyTxSafe runs ApplyTx with a panic firewall. Building a block executes untrusted
// mempool bytecode through the VM; a panic in this goroutine would crash the miner, so
// a panic degrades to a skipped tx (the trial state is a copy, discarded on error).
func applyTxSafe(s *state.State, tx *types.Transaction, coinbase common.Address, gasLimit uint64) (used, status uint64, logs []*common.Log, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in ApplyTx: %v", r)
		}
	}()
	return state.ApplyTx(s, tx, coinbase, gasLimit)
}

// errMiningAborted is returned by Build when a competing block arrives and the
// nonce search is abandoned. Not a failure: the caller rebuilds on the new head.
var errMiningAborted = errors.New("core: mining aborted")

// IsMiningAborted reports whether an error is the benign abort signal, so the node
// loop skips it quietly instead of logging a fault.
func IsMiningAborted(err error) bool { return errors.Is(err, errMiningAborted) }

// Producer builds blocks.
type Producer struct {
	onBlock func(*types.Block)

	bc        *Blockchain
	pool      *mempool.Mempool
	coinbase  common.Address
	blocksWon atomic.Uint64 // blocks this producer has sealed and committed (this run)
}

// Coinbase is the address this producer mines rewards to.
func (p *Producer) Coinbase() common.Address { return p.coinbase }

// BlocksWon is how many blocks this producer has sealed since the process started.
func (p *Producer) BlocksWon() uint64 { return p.blocksWon.Load() }

// SetOnBlock registers a callback fired after a block is inserted into the local
// chain. A hook rather than a direct dependency, so core need not import p2p. It
// fires after insertion, so a block that fails to insert is never announced.
func (p *Producer) SetOnBlock(fn func(*types.Block)) { p.onBlock = fn }

func NewProducer(bc *Blockchain, pool *mempool.Mempool, coinbase common.Address) *Producer {
	return &Producer{bc: bc, pool: pool, coinbase: coinbase}
}

// Build assembles a candidate block on top of head. It executes the transactions
// to obtain the state root then discards that state; InsertBlock executes them
// again. Deliberate: the producer must not hand itself a state the validator did
// not derive.
func (p *Producer) Build() (*types.Block, error) {
	parent := p.bc.Head()
	snapshot := p.bc.StateSnapshot()

	gasLimit := parent.Header.GasLimit
	txs := p.pool.Pending(snapshot, gasLimit)

	// Timestamps must strictly increase; wall clock can go backwards (NTP, VM
	// suspend), so clamp rather than emit an invalid block.
	ts := time.Now().UnixMilli()
	if ts <= parent.Header.Timestamp {
		ts = parent.Header.Timestamp + 1
	}

	header := &types.Header{
		ParentHash: parent.Hash(),
		Height:     parent.Height() + 1,
		Timestamp:  ts,
		GasLimit:   gasLimit,
		Proposer:   p.coinbase,
		// Difficulty is derived by LWMA over the recent window, never chosen; a
		// validator recomputes it and rejects a disagreeing block.
		Difficulty: p.bc.RequiredDifficulty(parent.Header),
	}

	working := snapshot.Copy()
	// Set the block context so NUMBER/TIMESTAMP/DIFFICULTY read real values. A
	// validator sets the same values from this header in ApplyBlock, so the state
	// root matches. Timestamp ms -> s.
	working.SetBlockContext(header.Height, uint64(header.Timestamp)/1000, header.Difficulty)
	var gasUsed uint64
	included := make([]*types.Transaction, 0, len(txs))
	receipts := make([]*types.Receipt, 0, len(txs))

	for _, tx := range txs {
		// Try each tx against a copy; drop a failing one and continue. Skipping a tx
		// is legitimate here because this decides block contents, not validation.
		trial := working.Copy()
		// A reverted call returns no error (it is an includable tx that consumed
		// gas), so status, not err, carries success/failure into the receipt.
		used, status, logs, err := applyTxSafe(trial, tx, p.coinbase, gasLimit)
		if err != nil {
			continue
		}
		if gasUsed+used > gasLimit {
			break
		}
		working = trial
		gasUsed += used
		included = append(included, tx)
		receipts = append(receipts, &types.Receipt{
			Status:            status,
			GasUsed:           used,
			CumulativeGasUsed: gasUsed,
			Logs:              logs,
		})
	}

	// Mint the block reward before rooting, exactly as ApplyBlock does, or the
	// published root will not match the validator's. Issuance is once per block,
	// 100% to the proposer, not per tx.
	state.CreditBlockReward(working, p.coinbase, header.Height)

	header.TxRoot = types.TxRoot(included)
	header.ReceiptRoot = types.ReceiptRoot(receipts)
	header.StateRoot = working.Root()
	header.GasUsed = gasUsed

	// Grind the nonce until the header hashes under target. Everything above is
	// fixed; the nonce is the only free field. Abort if a competing block replaces
	// the parent we built on while we grind — finishing would only produce a block
	// the fork choice discards, wasting the work. A watcher closes stop when the
	// head moves off parent; done stops the watcher once mining returns either way.
	stop := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if p.bc.Head().Hash() != parent.Hash() {
					close(stop)
					return
				}
			}
		}
	}()
	if !mine(header, stop) {
		return nil, errMiningAborted
	}

	return &types.Block{Header: header, Txs: included}, nil
}

// Commit inserts an already-built block, drains its txs from the pool, and
// announces it. Split from Seal so a caller can build a candidate, decide whether
// it is worth committing (e.g. skip an empty block), then commit.
func (p *Producer) Commit(block *types.Block) error {
	if err := p.bc.InsertBlock(block); err != nil {
		return err
	}
	p.blocksWon.Add(1)
	p.pool.Remove(block.Txs)
	if p.onBlock != nil {
		p.onBlock(block) // announce only once it is unambiguously ours
	}
	return nil
}

// Seal builds a block and commits it, the all-in-one path used by tests and simple
// callers.
func (p *Producer) Seal() (*types.Block, error) {
	block, err := p.Build()
	if err != nil {
		return nil, err
	}
	if err := p.Commit(block); err != nil {
		return nil, err
	}
	return block, nil
}
