package p2p

import (
	"encoding/json"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/state"
	"lxs/store"
	"lxs/types"
)

const testChainID = 1337

func testGenesis(t *testing.T, funded ...common.Address) *core.Genesis {
	t.Helper()
	alloc := map[common.Address]*core.BigStr{}
	total := new(big.Int)
	for _, a := range funded {
		v := common.LXS(1_000_000)
		alloc[a] = &core.BigStr{Int: v}
		total.Add(total, v)
	}
	return &core.Genesis{
		Name: "LXS", ChainID: testChainID, Timestamp: 1700000000000,
		GasLimit: 30_000_000, Alloc: alloc, TotalSupply: &core.BigStr{Int: total},
	}
}

type testNode struct {
	id PeerID
	bc *core.Blockchain
	g  *Gossip
	n  *InProc
}

func newTestNode(t *testing.T, sw *Switch, id PeerID, gen *core.Genesis, opts ...Option) *testNode {
	t.Helper()
	bc, err := core.NewBlockchain(store.NewMemory(), gen, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	n := sw.Join(id)
	g, err := NewGossip(n, bc, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &testNode{id: id, bc: bc, g: g, n: n}
}

// forger builds a valid block on an arbitrary parent, tracking its own state.
// The producer cannot: a node only builds on its own head. Tests play the
// network.
type forger struct {
	t        *testing.T
	tip      *types.Block
	st       *state.State
	proposer common.Address
}

func newForger(t *testing.T, bc *core.Blockchain, parent *types.Block) *forger {
	t.Helper()
	st, err := bc.StateAt(parent.Hash())
	if err != nil {
		t.Fatal(err)
	}
	k, _ := crypto.GenerateKey()
	return &forger{t: t, tip: parent, st: st, proposer: k.Address()}
}

func (f *forger) next(txs ...*types.Transaction) *types.Block {
	f.t.Helper()
	h := &types.Header{
		ParentHash: f.tip.Hash(),
		Height:     f.tip.Height() + 1,
		// One target interval per block: an on-target block leaves difficulty
		// unchanged (adjustment factor 0), so a difficulty-1 parent derives a
		// difficulty-1 child and the pin below keeps matching InsertBlock.
		Timestamp: f.tip.Header.Timestamp + core.TargetBlockTime,
		GasLimit:  f.tip.Header.GasLimit,
		Proposer:  f.proposer,
		// Difficulty pinned at the floor (1): the target is the whole hash space,
		// so nonce 0 is a valid proof and no grinding is needed.
		Difficulty: 1,
	}
	var gasUsed uint64
	receipts := make([]*types.Receipt, 0, len(txs))
	for _, tx := range txs {
		used, status, gLogs, err := state.ApplyTx(f.st, tx, f.proposer, h.GasLimit)
		if err != nil {
			f.t.Fatalf("invalid tx in forged block: %v", err)
		}
		gasUsed += used
		receipts = append(receipts, &types.Receipt{
			Status: status, GasUsed: used, CumulativeGasUsed: gasUsed, Logs: gLogs,
		})
	}
	// Match ApplyBlock: mint the block reward (100% to the proposer) before
	// rooting, or the forged block's state root will not match what a validating
	// node derives.
	state.CreditBlockReward(f.st, f.proposer, h.Height)

	h.TxRoot = types.TxRoot(txs)
	h.ReceiptRoot = types.ReceiptRoot(receipts)
	h.StateRoot = f.st.Root()
	h.GasUsed = gasUsed
	blk := &types.Block{Header: h, Txs: txs}
	f.tip = blk
	return blk
}

// One producer, three followers, on a network that duplicates every message and
// shuffles delivery. All four must end up on the same head.
func TestFourNodesConvergeOnOneProducer(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())

	// 2 extra copies of everything: GossipSub is a mesh, the same block arrives
	// from every meshed peer.
	sw := NewSwitch(SwitchConfig{Duplicates: 2, Shuffle: true, Seed: 1})

	producer := newTestNode(t, sw, "node0", gen)
	followers := []*testNode{
		newTestNode(t, sw, "node1", gen),
		newTestNode(t, sw, "node2", gen),
		newTestNode(t, sw, "node3", gen),
	}

	f := newForger(t, producer.bc, producer.bc.Head())
	for i := 0; i < 20; i++ {
		blk := f.next()
		// Insert locally first, then announce: announcing a block we failed to
		// insert advertises something we do not have.
		if err := producer.bc.InsertBlock(blk); err != nil {
			t.Fatalf("producer could not insert its own block %d: %v", i, err)
		}
		if err := producer.g.Announce(blk); err != nil {
			t.Fatal(err)
		}
	}

	want := producer.bc.Head().Hash()
	if producer.bc.Head().Height() != 20 {
		t.Fatalf("producer height: got %d want 20", producer.bc.Head().Height())
	}

	for _, n := range followers {
		if n.bc.Head().Height() != 20 {
			t.Fatalf("%s height: got %d want 20", n.id, n.bc.Head().Height())
		}
		if n.bc.Head().Hash() != want {
			t.Fatalf("%s head: got %s want %s — the network partitioned",
				n.id, n.bc.Head().Hash().Hex(), want.Hex())
		}
		// Every follower must have executed the blocks, not just stored them:
		// same head hash but a different state root means two nodes agreeing on
		// history and disagreeing on balances.
		if n.bc.StateSnapshot().Root() != n.bc.Head().Header.StateRoot {
			t.Fatalf("%s state root does not match its head", n.id)
		}
	}
}

// Duplicates must be cheap and harmless. A healthy mesh delivers the same block
// many times; if that were expensive, a healthy network would look like an attack.
func TestDuplicatesAreDetectedAndCheap(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Duplicates: 4, Seed: 2})

	producer := newTestNode(t, sw, "node0", gen)
	follower := newTestNode(t, sw, "node1", gen)

	f := newForger(t, producer.bc, producer.bc.Head())
	blk := f.next()
	producer.bc.InsertBlock(blk)
	producer.g.Announce(blk)

	s := follower.g.Snapshot()
	if s.Received != 5 {
		t.Fatalf("received: got %d want 5", s.Received)
	}
	if s.Accepted != 1 {
		t.Fatalf("a duplicated block was applied %d times — state would be double-counted", s.Accepted)
	}
	// The cheap path must catch them. InsertBlock would reject duplicates anyway;
	// the claim is that a healthy mesh does not cost a lookup and a validation
	// entry per copy.
	if s.FastDuplicate != 4 {
		t.Fatalf("duplicates caught by the cheap HasBlock check: got %d want 4", s.FastDuplicate)
	}
	if s.LateDuplicate != 0 {
		t.Fatalf("%d duplicates reached InsertBlock — the cheap check is not doing its job, "+
			"and a healthy network would look like an attack", s.LateDuplicate)
	}
	if follower.bc.Head().Hash() != blk.Hash() {
		t.Fatal("follower did not accept the block")
	}
}

// A child arriving before its parent is normal (gossip has no ordering). It must
// be queued and resolved, not discarded.
func TestChildBeforeParentIsResolved(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 3})

	producer := newTestNode(t, sw, "node0", gen)
	follower := newTestNode(t, sw, "node1", gen)

	// Build 5 blocks without announcing any.
	f := newForger(t, producer.bc, producer.bc.Head())
	var blocks []*types.Block
	for i := 0; i < 5; i++ {
		b := f.next()
		if err := producer.bc.InsertBlock(b); err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, b)
	}

	// Announce in reverse order: every block arrives before its parent, so
	// nothing can be applied until the last message.
	for i := len(blocks) - 1; i >= 1; i-- {
		if err := producer.g.Announce(blocks[i]); err != nil {
			t.Fatal(err)
		}
	}
	if follower.bc.Head().Height() != 0 {
		t.Fatalf("follower applied a block with no parent: height %d", follower.bc.Head().Height())
	}
	if follower.g.OrphanCount() != 4 {
		t.Fatalf("orphans queued: got %d want 4", follower.g.OrphanCount())
	}

	// One block arrives and the whole chain must unlock.
	if err := producer.g.Announce(blocks[0]); err != nil {
		t.Fatal(err)
	}

	if follower.bc.Head().Hash() != blocks[4].Hash() {
		t.Fatalf("the orphan chain did not resolve: follower at height %d",
			follower.bc.Head().Height())
	}
	if follower.g.OrphanCount() != 0 {
		t.Fatalf("orphans left behind after resolution: %d", follower.g.OrphanCount())
	}
}

// A peer can send unlimited blocks with random parent hashes that never resolve.
// Unbounded, that exhausts memory; the queue must stay bounded.
func TestOrphanQueueIsBounded(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 4})

	const limit = 16
	const perPeer = 5
	attacker := newTestNode(t, sw, "attacker", gen)
	victim := newTestNode(t, sw, "victim", gen, WithMaxOrphans(limit), WithMaxOrphansPerPeer(perPeer))

	// 500 well-formed blocks whose parents do not exist and never will, all from
	// one peer.
	for i := 0; i < 500; i++ {
		var fake common.Hash
		fake[0] = byte(i)
		fake[1] = byte(i >> 8)
		fake[31] = 0xff
		blk := &types.Block{Header: &types.Header{
			ParentHash: fake,
			Height:     uint64(i + 1),
			Timestamp:  1700000001000,
			GasLimit:   30_000_000,
			TxRoot:     types.TxRoot(nil),
		}}
		data, _ := json.Marshal(blockMessage{Block: blk})
		attacker.n.Publish(TopicBlocks, data)
	}

	// Per-peer bounding means a single peer cannot even fill the queue, let alone
	// evict anyone: it is capped at its own quota.
	if got := victim.g.OrphanCount(); got > perPeer {
		t.Fatalf("one peer holds %d orphans with a per-peer cap of %d — this is the OOM", got, perPeer)
	}
	// Everything past its quota was refused (dropped and penalised), not queued.
	if victim.g.Snapshot().Rejected == 0 {
		t.Fatal("the attacker's over-quota orphans were not refused")
	}
	// And the victim is still usable.
	if victim.bc.Head().Height() != 0 {
		t.Fatal("junk moved the head")
	}
}

// One peer cannot starve another: a flood from A must not evict B's orphan.
func TestOrphanQueueIsPerPeerFair(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 41})

	const limit = 8
	const perPeer = 3
	flooder := newTestNode(t, sw, "flooder", gen)
	honest := newTestNode(t, sw, "honest", gen)
	victim := newTestNode(t, sw, "victim", gen, WithMaxOrphans(limit), WithMaxOrphansPerPeer(perPeer))

	orphan := func(n *testNode, tag byte) common.Hash {
		var fake common.Hash
		fake[0] = tag
		blk := &types.Block{Header: &types.Header{
			ParentHash: fake, Height: 1, Timestamp: 1700000001000,
			GasLimit: 30_000_000, TxRoot: types.TxRoot(nil),
		}}
		data, _ := json.Marshal(blockMessage{Block: blk})
		n.n.Publish(TopicBlocks, data)
		return blk.Hash()
	}

	// Honest peer parks one orphan; the flooder tries to bury it under a hundred.
	orphan(honest, 0x01)
	for i := 0; i < 100; i++ {
		orphan(flooder, byte(0x80+i%120))
	}

	// The flooder is capped at its quota; the honest orphan is untouched.
	if got := victim.g.OrphanCount(); got > perPeer+1 {
		t.Fatalf("queue holds %d, want at most one honest + %d flooder", got, perPeer)
	}
	// The honest peer never crosses the ban threshold; the flooder is punished.
	if victim.g.Snapshot().Rejected == 0 {
		t.Fatal("the flooder's over-quota orphans were not refused")
	}
}

// Re-delivering the same orphan must not grow the queue. Gossip re-delivers it
// while its parent is missing, and a queue that grows on duplicates has no bound.
func TestDuplicateOrphansDoNotGrowTheQueue(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Duplicates: 9, Seed: 5})

	sender := newTestNode(t, sw, "a", gen)
	recv := newTestNode(t, sw, "b", gen)

	var fake common.Hash
	fake[0] = 0xab
	blk := &types.Block{Header: &types.Header{
		ParentHash: fake, Height: 1, Timestamp: 1700000001000,
		GasLimit: 30_000_000, TxRoot: types.TxRoot(nil),
	}}
	data, _ := json.Marshal(blockMessage{Block: blk})
	sender.n.Publish(TopicBlocks, data) // delivered 10 times

	if got := recv.g.OrphanCount(); got != 1 {
		t.Fatalf("the same orphan was queued %d times", got)
	}
}

// A forged block must be rejected and must not become head, even arriving from a
// connected peer.
func TestForgedBlockIsRejectedAndDoesNotPropagate(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 6})

	honest := newTestNode(t, sw, "honest", gen)
	liar := newTestNode(t, sw, "liar", gen)

	f := newForger(t, liar.bc, liar.bc.Head())
	blk := f.next()
	// Lie about the state root — claim money moved that did not.
	blk.Header.StateRoot = common.Hash{0xba, 0xad, 0xf0, 0x0d}

	data, _ := json.Marshal(blockMessage{Block: blk})
	liar.n.Publish(TopicBlocks, data)

	if honest.bc.Head().Height() != 0 {
		t.Fatal("a forged block became head")
	}
	if honest.bc.HasBlock(blk.Hash()) {
		t.Fatal("a forged block was stored")
	}
	if honest.g.Snapshot().Rejected != 1 {
		t.Fatalf("rejections: got %d want 1", honest.g.Snapshot().Rejected)
	}
	// The handler must have returned an error: peer scoring hangs off it.
	// Swallowing it would leave nothing to score.
	if len(honest.n.HandlerErrors()) == 0 {
		t.Fatal("the handler swallowed a rejection instead of surfacing it")
	}
}

func TestGarbageIsRejectedNotPanicked(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 7})

	sender := newTestNode(t, sw, "a", gen)
	recv := newTestNode(t, sw, "b", gen)

	for _, junk := range [][]byte{
		[]byte(""),
		[]byte("{"),
		[]byte("null"),
		[]byte(`{"block":null}`),
		[]byte(`{"block":{}}`),
		[]byte(`{"block":{"header":null}}`),
		[]byte("\x00\x01\x02\xff"),
		[]byte(`{"block":{"header":{"height":"not a number"}}}`),
	} {
		sender.n.Publish(TopicBlocks, junk)
	}

	if recv.bc.Head().Height() != 0 {
		t.Fatal("garbage moved the head")
	}
	if recv.g.Snapshot().Rejected != 8 {
		t.Fatalf("rejected: got %d want 8", recv.g.Snapshot().Rejected)
	}
}

func TestOversizedMessageIsRejectedBeforeParsing(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 8})

	sender := newTestNode(t, sw, "a", gen)
	recv := newTestNode(t, sw, "b", gen)

	huge := make([]byte, maxBlockMessage+1)
	sender.n.Publish(TopicBlocks, huge)

	if recv.g.Snapshot().Rejected != 1 {
		t.Fatal("an oversized message was not rejected")
	}
	if recv.g.Snapshot().Accepted != 0 {
		t.Fatal("an oversized message was processed")
	}
}

// The late-joiner case lives in sync_test.go as TestLateJoinerCatchesUpViaSync:
// a node that joins after blocks were produced cannot catch up via gossip alone
// (gossip carries only what is published from now on), so the block it cannot
// place arms a sync that closes the gap. Gossip-only non-catch-up is still
// covered here by the orphan tests: a block with an unknown parent is parked,
// never applied.

// The orphan queue must be bounded by BYTES, not only count. An orphan's real
// difficulty cannot be verified until its parent arrives, so a peer with no
// hashpower can park blocks that merely CLAIM a low difficulty, each carrying up to
// a full message of attacker-chosen calldata. Under a count-only bound, 256 such
// blocks × ~1 MiB is an OOM on the target VPS. This floods big-calldata orphans and
// asserts the parked memory stays under the byte budget regardless of count.
func TestOrphanQueueIsByteBounded(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 7})

	const budget = 4 << 20 // 4 MiB
	attacker := newTestNode(t, sw, "attacker", gen)
	// High count cap so the BYTE bound is the binding constraint, not the count.
	victim := newTestNode(t, sw, "victim", gen,
		WithMaxOrphans(10000), WithMaxOrphansPerPeer(10000), WithMaxOrphanBytes(budget))

	bigData := make([]byte, 512*1024) // 512 KiB calldata per orphan
	for i := 0; i < 200; i++ {        // 200 × 512 KiB = 100 MiB attempted
		var fake common.Hash
		fake[0], fake[1], fake[31] = byte(i), byte(i>>8), 0xfe
		tx := types.NewTransaction(1, 0, common.Address{0x01}, big.NewInt(0), 21000, big.NewInt(1), bigData)
		blk := &types.Block{Header: &types.Header{
			ParentHash: fake, Height: uint64(i + 1), Timestamp: 1700000001000, GasLimit: 30_000_000,
		}, Txs: []*types.Transaction{tx}}
		data, _ := json.Marshal(blockMessage{Block: blk})
		attacker.n.Publish(TopicBlocks, data)
	}

	if got := victim.g.OrphanBytes(); got > budget {
		t.Fatalf("orphan queue holds %d bytes, over the %d budget — the OOM vector is open", got, budget)
	}
	// The victim is still alive and did not accept any junk.
	if victim.bc.Head().Height() != 0 {
		t.Fatal("junk moved the head")
	}
}

// A single orphan larger than the entire byte budget can never fit; it must be
// dropped (and the sender penalised), not loop forever draining the queue.
func TestSingleOversizedOrphanIsDropped(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 8})

	attacker := newTestNode(t, sw, "attacker", gen)
	victim := newTestNode(t, sw, "victim", gen, WithMaxOrphanBytes(1<<20)) // 1 MiB budget

	huge := make([]byte, 2<<20) // 2 MiB, bigger than the whole budget
	tx := types.NewTransaction(1, 0, common.Address{0x01}, big.NewInt(0), 21000, big.NewInt(1), huge)
	var fake common.Hash
	fake[0] = 0xaa
	blk := &types.Block{Header: &types.Header{
		ParentHash: fake, Height: 1, Timestamp: 1700000001000, GasLimit: 30_000_000,
	}, Txs: []*types.Transaction{tx}}
	data, _ := json.Marshal(blockMessage{Block: blk})
	attacker.n.Publish(TopicBlocks, data)

	if got := victim.g.OrphanCount(); got != 0 {
		t.Fatalf("oversized orphan was queued (count=%d), want it dropped", got)
	}
	if got := victim.g.OrphanBytes(); got != 0 {
		t.Fatalf("orphan bytes = %d after an oversized drop, want 0", got)
	}
}
