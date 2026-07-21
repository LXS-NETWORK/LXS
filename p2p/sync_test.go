package p2p

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/store"
	"lxs/types"
)

// A header dated a few seconds ahead of our clock is honest drift — a peer whose
// freshly-mined tip beats our clock — not a lie. Block gossip already treats this as
// a non-penalised "deferred"; sync must classify it the same, as ErrFutureHeader and
// NOT the penalising ErrHeaderChain, so an honest peer is not scored toward a ban for
// clock skew.
func TestSyncFutureDatedTipIsNotPenalised(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 71})
	bc := newBC(t, gen)
	s, err := NewSyncer(sw.Join("me"), bc)
	if err != nil {
		t.Fatal(err)
	}

	h := &types.Header{
		Height:     1,
		ParentHash: bc.Head().Hash(), // links to our genesis head
		Timestamp:  time.Now().UnixMilli() + 10*core.MaxFutureDriftMs,
		Difficulty: core.MinDifficulty,
	}
	err = s.checkHeaderChain([]*types.Header{h}, 1)
	if !errors.Is(err, ErrFutureHeader) {
		t.Fatalf("future-dated tip classified as %v, want ErrFutureHeader", err)
	}
	if errors.Is(err, ErrHeaderChain) {
		t.Fatal("future-dated tip must NOT be ErrHeaderChain — that path penalises the peer for honest clock drift")
	}
}

func newBC(t *testing.T, gen *core.Genesis) *core.Blockchain {
	t.Helper()
	bc, err := core.NewBlockchain(store.NewMemory(), gen, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return bc
}

// buildChain inserts n valid blocks onto bc's head.
func buildChain(t *testing.T, bc *core.Blockchain, n int) {
	t.Helper()
	f := newForger(t, bc, bc.Head())
	for i := 0; i < n; i++ {
		if err := bc.InsertBlock(f.next()); err != nil {
			t.Fatalf("building chain at %d: %v", i, err)
		}
	}
}

// syncNode wires both halves: gossip for propagation, a syncer for catch-up, and
// gossip's gap signal driving the syncer. SyncFrom runs synchronously here so the
// test needs no sleeps; the real node runs it in a goroutine, a transport
// difference, not a logic one.
type syncNode struct {
	bc *core.Blockchain
	g  *Gossip
	s  *Syncer
	n  *InProc
}

func newSyncNode(t *testing.T, sw *Switch, id PeerID, gen *core.Genesis) *syncNode {
	t.Helper()
	bc := newBC(t, gen)
	n := sw.Join(id)
	s, err := NewSyncer(n, bc)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGossip(n, bc, WithGapHandler(func(p PeerID) { s.SyncFrom(p) }))
	if err != nil {
		t.Fatal(err)
	}
	return &syncNode{bc: bc, g: g, s: s, n: n}
}

// Happy path: a node at genesis pulls a whole chain from a peer and ends on the
// same head hash.
func TestSyncCatchesUpFromGenesis(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 20})

	serverBC := newBC(t, gen)
	buildChain(t, serverBC, 10)
	if _, err := NewSyncer(sw.Join("server"), serverBC); err != nil {
		t.Fatal(err)
	}

	clientBC := newBC(t, gen)
	client, err := NewSyncer(sw.Join("client"), clientBC)
	if err != nil {
		t.Fatal(err)
	}

	client.SyncFrom("server")

	if got := clientBC.Head().Height(); got != 10 {
		t.Fatalf("client height %d, want 10", got)
	}
	if clientBC.Head().Hash() != serverBC.Head().Hash() {
		t.Fatalf("client head %s != server head %s",
			clientBC.Head().Hash().Hex(), serverBC.Head().Hash().Hex())
	}
}

// A chain longer than one batch must still fully sync: the driver loops.
func TestSyncSpansMultipleBatches(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 21})

	serverBC := newBC(t, gen)
	buildChain(t, serverBC, maxHeadersPerRequest+50) // forces a second round
	if _, err := NewSyncer(sw.Join("server"), serverBC); err != nil {
		t.Fatal(err)
	}

	clientBC := newBC(t, gen)
	client, err := NewSyncer(sw.Join("client"), clientBC)
	if err != nil {
		t.Fatal(err)
	}

	client.SyncFrom("server")

	if clientBC.Head().Hash() != serverBC.Head().Hash() {
		t.Fatalf("client at %d did not reach server at %d",
			clientBC.Head().Height(), serverBC.Head().Height())
	}
}

// registerRaw wires an arbitrary sync handler onto a fresh node, so a test can
// play a hostile peer that answers however it likes.
func registerRaw(t *testing.T, sw *Switch, id PeerID, h RequestHandler) {
	t.Helper()
	if err := sw.Join(id).SetRequestHandler(ProtoSync, h); err != nil {
		t.Fatal(err)
	}
}

// A peer serving a header chain that does not attach to what we hold must be
// refused, and we must not advance a block on its word.
func TestSyncRejectsUnlinkedHeaders(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 22})

	// bodyRequests counts how many times the liar was asked for bodies. It must
	// stay zero for a chain that fails the cheap header check: no body bandwidth
	// on headers that do not link. Asserting only "client stayed at 0" is too
	// weak (InsertBlock would refuse anyway); this isolates the header-stage guard.
	bodyRequests := 0
	registerRaw(t, sw, "liar", func(_ PeerID, req []byte) ([]byte, error) {
		var r syncRequest
		if err := json.Unmarshal(req, &r); err != nil {
			return nil, err
		}
		if r.Kind == kindBodies {
			bodyRequests++
			return json.Marshal(syncResponse{})
		}
		// Height claims to start where asked, but its parent hangs off no block
		// we have.
		h := &types.Header{Height: r.From, ParentHash: common.Hash{0xDE, 0xAD}}
		return json.Marshal(syncResponse{Headers: []*types.Header{h}})
	})

	clientBC := newBC(t, gen)
	client, err := NewSyncer(sw.Join("client"), clientBC)
	if err != nil {
		t.Fatal(err)
	}

	client.SyncFrom("liar")

	if got := clientBC.Head().Height(); got != 0 {
		t.Fatalf("client advanced to %d on an unlinked chain — it must stay at 0", got)
	}
	if bodyRequests != 0 {
		t.Fatalf("client fetched bodies %d times for an unlinked header chain — "+
			"headers-first must reject before spending body bandwidth", bodyRequests)
	}
}

// A peer returning more headers than the protocol allows is misbehaving; its
// whole answer is refused rather than partially trusted.
func TestSyncRejectsOversizedHeaderBatch(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 23})

	registerRaw(t, sw, "flood", func(_ PeerID, req []byte) ([]byte, error) {
		var r syncRequest
		if err := json.Unmarshal(req, &r); err != nil {
			return nil, err
		}
		if r.Kind != kindHeaders {
			return json.Marshal(syncResponse{})
		}
		hs := make([]*types.Header, maxHeadersPerRequest+1)
		for i := range hs {
			hs[i] = &types.Header{Height: r.From + uint64(i)}
		}
		return json.Marshal(syncResponse{Headers: hs})
	})

	clientBC := newBC(t, gen)
	client, err := NewSyncer(sw.Join("client"), clientBC)
	if err != nil {
		t.Fatal(err)
	}

	client.SyncFrom("flood")

	if got := clientBC.Head().Height(); got != 0 {
		t.Fatalf("client advanced to %d on an oversized batch — it must stay at 0", got)
	}
}

// Bait and switch: advertise honest, linking headers, then deliver a body that
// hashes to something else. The body must be refused and the chain not advanced.
func TestSyncRejectsSwappedBody(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 24})

	// An honest chain to lift real, linking headers from.
	honestBC := newBC(t, gen)
	buildChain(t, honestBC, 3)

	// An unrelated block whose hash is in nobody's request set.
	otherBC := newBC(t, gen)
	otherF := newForger(t, otherBC, otherBC.Head())
	otherBlock := otherF.next()

	registerRaw(t, sw, "switcher", func(_ PeerID, req []byte) ([]byte, error) {
		var r syncRequest
		if err := json.Unmarshal(req, &r); err != nil {
			return nil, err
		}
		switch r.Kind {
		case kindHeaders:
			var out syncResponse
			for i := uint64(0); i < r.Max; i++ {
				b, err := honestBC.BlockByHeight(r.From + i)
				if err != nil {
					break
				}
				out.Headers = append(out.Headers, b.Header)
			}
			return json.Marshal(out)
		default: // bodies: hand back the wrong block
			return json.Marshal(syncResponse{Blocks: []*types.Block{otherBlock}})
		}
	})

	clientBC := newBC(t, gen)
	client, err := NewSyncer(sw.Join("client"), clientBC)
	if err != nil {
		t.Fatal(err)
	}

	client.SyncFrom("switcher")

	if got := clientBC.Head().Height(); got != 0 {
		t.Fatalf("client advanced to %d on a swapped body — it must stay at 0", got)
	}
}

// A late node cannot backfill via gossip alone, but the orphan off the gossip
// path arms sync, and the late node closes the whole gap it missed.
func TestLateJoinerCatchesUpViaSync(t *testing.T) {
	k, _ := crypto.GenerateKey()
	gen := testGenesis(t, k.Address())
	sw := NewSwitch(SwitchConfig{Seed: 9})

	producer := newSyncNode(t, sw, "node0", gen)
	early := newSyncNode(t, sw, "node1", gen)

	f := newForger(t, producer.bc, producer.bc.Head())
	for i := 0; i < 5; i++ {
		b := f.next()
		producer.bc.InsertBlock(b)
		producer.g.Announce(b)
	}
	if early.bc.Head().Height() != 5 {
		t.Fatal("the early follower should have kept up via gossip")
	}

	// Joins now, having missed blocks 1..5 entirely.
	late := newSyncNode(t, sw, "node4", gen)

	// One fresh block arrives. Its parent (block 5) is unknown to the late node,
	// so gossip parks it and signals the gap, which drives sync.
	b := f.next()
	producer.bc.InsertBlock(b)
	producer.g.Announce(b)

	if got := late.bc.Head().Height(); got != 6 {
		t.Fatalf("late joiner at height %d, want 6 — sync should have closed the gap", got)
	}
	if late.bc.Head().Hash() != producer.bc.Head().Hash() {
		t.Fatal("late joiner converged to the wrong head")
	}
	if early.bc.Head().Hash() != producer.bc.Head().Hash() {
		t.Fatal("the early follower fell behind")
	}
}
