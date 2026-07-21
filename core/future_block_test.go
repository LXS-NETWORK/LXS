package core

import (
	"errors"
	"testing"
	"time"

	"lxs/mempool"
	"lxs/store"
)

// A block dated too far ahead of a node's wall clock is refused, but only for now.
// Timestamps drive difficulty retargeting, so without this ceiling a miner could
// post-date a block to ease its own difficulty. Retriable: the block is not stored
// and is accepted once the clock catches up.
func TestFutureBlockRefusedThenAcceptedWhenClockCatchesUp(t *testing.T) {
	g := testGenesis()

	// Mine a real, valid-PoW block on a normal chain (uses the real wall clock,
	// so its timestamp is ~now).
	src := NewMemBlockchain(g)
	miner := newKey(t)
	blk, err := NewProducer(src, mempool.New(10), miner.Address()).Seal()
	if err != nil {
		t.Fatal(err)
	}
	blkTs := blk.Header.Timestamp

	// A second chain whose clock is set before the block's timestamp by more than
	// the drift ceiling, so from this node's view the block is in the future.
	clk := time.UnixMilli(blkTs - MaxFutureDriftMs - 5_000)
	bc, err := NewBlockchain(store.NewMemory(), g, Options{Now: func() time.Time { return clk }})
	if err != nil {
		t.Fatal(err)
	}

	if err := bc.InsertBlock(blk); !errors.Is(err, ErrFutureBlock) {
		t.Fatalf("a too-far-future block must be refused with ErrFutureBlock, got %v", err)
	}
	// refused, not stored, so it can be re-offered later (retriable, not banned).
	if bc.HasBlock(blk.Hash()) {
		t.Fatal("a refused future block must not be stored — it has to remain re-offerable")
	}

	// Advance the clock to within the drift window: now the block is acceptable.
	clk = time.UnixMilli(blkTs - MaxFutureDriftMs + 1_000) // block is ~drift-1s ahead
	if err := bc.InsertBlock(blk); err != nil {
		t.Fatalf("once the clock catches up the block must be accepted, got %v", err)
	}
	if !bc.HasBlock(blk.Hash()) {
		t.Fatal("the now-valid block should be stored")
	}
}

// A block a little ahead of the clock (ordinary NTP skew, under the ceiling) is
// accepted immediately; the rule must not reject honest miners whose clocks run a
// few seconds fast.
func TestBlockWithinDriftAccepted(t *testing.T) {
	g := testGenesis()
	src := NewMemBlockchain(g)
	miner := newKey(t)
	blk, err := NewProducer(src, mempool.New(10), miner.Address()).Seal()
	if err != nil {
		t.Fatal(err)
	}

	// clock is 5s behind the block, well inside the 15s ceiling.
	clk := time.UnixMilli(blk.Header.Timestamp - 5_000)
	bc, err := NewBlockchain(store.NewMemory(), g, Options{Now: func() time.Time { return clk }})
	if err != nil {
		t.Fatal(err)
	}
	if err := bc.InsertBlock(blk); err != nil {
		t.Fatalf("a block only 5s ahead (within drift) must be accepted, got %v", err)
	}
}
