package p2p

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/types"
)

func key(t *testing.T) *crypto.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type txNode struct {
	bc   *core.Blockchain
	pool *mempool.Mempool
	tg   *TxGossip
	n    *InProc
}

func newTxNode(t *testing.T, sw *Switch, id PeerID, gen *core.Genesis, poolSize int) *txNode {
	t.Helper()
	bc := newBC(t, gen)
	pool := mempool.New(poolSize)
	n := sw.Join(id)
	tg, err := NewTxGossip(n, bc, pool)
	if err != nil {
		t.Fatal(err)
	}
	return &txNode{bc: bc, pool: pool, tg: tg, n: n}
}

func signedTx(t *testing.T, k *crypto.PrivateKey, nonce uint64, value, gasPrice int64) *types.Transaction {
	t.Helper()
	var to common.Address // burn address is fine; admission does not care where it goes
	tx := types.NewTransaction(testChainID, nonce, to, big.NewInt(value), types.IntrinsicGas, big.NewInt(gasPrice), nil)
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

// A valid transaction from a funded account reaches another node's pool.
func TestTxGossipPropagates(t *testing.T) {
	sender := key(t)
	gen := testGenesis(t, sender.Address())
	sw := NewSwitch(SwitchConfig{Seed: 30})

	from := newTxNode(t, sw, "from", gen, 8192)
	to := newTxNode(t, sw, "to", gen, 8192)

	tx := signedTx(t, sender, 0, 100, 1)
	if err := from.tg.Broadcast(tx); err != nil {
		t.Fatal(err)
	}

	if _, ok := to.pool.Get(tx.Hash()); !ok {
		t.Fatal("valid tx did not reach the receiver's pool")
	}
	if to.tg.Snapshot().Accepted != 1 {
		t.Fatalf("receiver accepted %d, want 1", to.tg.Snapshot().Accepted)
	}
}

// A tampered signature must not admit a transaction.
func TestTxGossipRejectsBadSignature(t *testing.T) {
	sender := key(t)
	gen := testGenesis(t, sender.Address())
	sw := NewSwitch(SwitchConfig{Seed: 31})

	from := newTxNode(t, sw, "from", gen, 8192)
	to := newTxNode(t, sw, "to", gen, 8192)

	tx := signedTx(t, sender, 0, 100, 1)
	// Zero the signature so recovery unambiguously fails (unsigned): a
	// stateless-invalid, forged tx. A one-byte tamper that recovers to a random
	// unfunded address is indistinguishable from a legit unfunded tx and is
	// covered by the unfunded-sender test (dropped, relayer not penalised).
	for i := range tx.Sig {
		tx.Sig[i] = 0
	}

	_ = from.tg.Broadcast(tx)

	if to.pool.Len() != 0 {
		t.Fatal("a tx with a broken signature was admitted")
	}
	// A provably-forged tx DOES penalise the relayer.
	if to.tg.Snapshot().Rejected != 1 {
		t.Fatalf("receiver rejected %d, want 1", to.tg.Snapshot().Rejected)
	}
}

// A transaction from an account with no balance is spam and must be refused
// before it takes a pool slot.
func TestTxGossipRejectsUnfundedSender(t *testing.T) {
	funded := key(t)
	gen := testGenesis(t, funded.Address()) // only `funded` has a balance
	sw := NewSwitch(SwitchConfig{Seed: 32})

	from := newTxNode(t, sw, "from", gen, 8192)
	to := newTxNode(t, sw, "to", gen, 8192)

	pauper := key(t) // not in the alloc: balance 0
	tx := signedTx(t, pauper, 0, 100, 1)

	_ = from.tg.Broadcast(tx)

	if to.pool.Len() != 0 {
		t.Fatal("a tx the sender cannot pay for was admitted — mempool spam floor is broken")
	}
	// The unfunded tx is dropped, but the relayer is not penalised: it forwarded
	// someone else's tx, and "cannot pay" is not its fault. Penalising it would
	// let an attacker get honest relayers banned by making them forward crafted
	// unfunded txs.
	snap := to.tg.Snapshot()
	if snap.Dropped != 1 {
		t.Fatalf("receiver dropped %d, want 1", snap.Dropped)
	}
	if snap.Rejected != 0 {
		t.Fatalf("receiver penalised the relayer %d times for an unfunded tx, want 0", snap.Rejected)
	}
}

// A transaction signed for a different chain must be refused.
func TestTxGossipRejectsWrongChain(t *testing.T) {
	sender := key(t)
	gen := testGenesis(t, sender.Address())
	sw := NewSwitch(SwitchConfig{Seed: 33})

	from := newTxNode(t, sw, "from", gen, 8192)
	to := newTxNode(t, sw, "to", gen, 8192)

	// Signed with the wrong chain id by the funded key: it clears the balance
	// gate but must die on the chain-id check.
	tx := types.NewTransaction(testChainID+1, 0, common.Address{}, big.NewInt(100), types.IntrinsicGas, big.NewInt(1), nil)
	if err := tx.Sign(sender); err != nil {
		t.Fatal(err)
	}

	_ = from.tg.Broadcast(tx)

	if to.pool.Len() != 0 {
		t.Fatal("a tx for another chain was admitted")
	}
}

// An oversized message is rejected on size alone, before decode. The payload is
// a valid, funded, otherwise-admissible tx padded past the cap with an ignored
// field: strip the cap and it would decode and be accepted, so this isolates the
// size gate rather than passing on unparseable junk.
func TestTxGossipRejectsOversizedMessage(t *testing.T) {
	sender := key(t)
	gen := testGenesis(t, sender.Address())
	sw := NewSwitch(SwitchConfig{Seed: 34})

	to := newTxNode(t, sw, "to", gen, 8192)
	raw := sw.Join("blob")

	tx := signedTx(t, sender, 0, 100, 1)
	// A valid txMessage plus a big ignored field: well-formed JSON that decodes
	// to a good tx, with only its size wrong.
	padded, err := json.Marshal(map[string]interface{}{
		"tx":  tx,
		"pad": strings.Repeat("A", maxTxMessage),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(padded) <= maxTxMessage {
		t.Fatalf("test bug: padded message is %d bytes, not over the cap", len(padded))
	}
	if err := raw.Publish(TopicTxs, padded); err != nil {
		t.Fatal(err)
	}

	if to.pool.Len() != 0 {
		t.Fatal("oversized message was admitted — the size cap did not fire")
	}
	if to.tg.Snapshot().Rejected != 1 {
		t.Fatalf("receiver rejected %d, want 1", to.tg.Snapshot().Rejected)
	}
}

// The same tx arriving many times (a mesh forwards constantly) is admitted once
// and counted as a duplicate thereafter, never re-added.
func TestTxGossipDeduplicates(t *testing.T) {
	sender := key(t)
	gen := testGenesis(t, sender.Address())
	// Two extra copies of everything: three deliveries per publish.
	sw := NewSwitch(SwitchConfig{Duplicates: 2, Seed: 35})

	from := newTxNode(t, sw, "from", gen, 8192)
	to := newTxNode(t, sw, "to", gen, 8192)

	tx := signedTx(t, sender, 0, 100, 1)
	_ = from.tg.Broadcast(tx)

	if to.pool.Len() != 1 {
		t.Fatalf("pool holds %d copies of one tx, want 1", to.pool.Len())
	}
	s := to.tg.Snapshot()
	if s.Accepted != 1 || s.Duplicate != 2 {
		t.Fatalf("accepted=%d duplicate=%d, want 1 and 2", s.Accepted, s.Duplicate)
	}
}

// A pool with a hard cap must never exceed it, however many valid transactions
// are pushed: the surplus is refused, not queued. Memory stays bounded even
// under a flood of otherwise-fine txs.
func TestTxGossipPoolCapBoundsMemory(t *testing.T) {
	const cap = 3
	senders := make([]*crypto.PrivateKey, 5)
	addrs := make([]common.Address, 5)
	for i := range senders {
		senders[i] = key(t)
		addrs[i] = senders[i].Address()
	}
	gen := testGenesis(t, addrs...)
	sw := NewSwitch(SwitchConfig{Seed: 36})

	from := newTxNode(t, sw, "from", gen, 8192)
	to := newTxNode(t, sw, "to", gen, cap)

	for i, s := range senders {
		// Distinct senders so every tx is valid and independent; only the cap
		// should stop them.
		_ = from.tg.Broadcast(signedTx(t, s, 0, int64(100+i), 1))
	}

	if to.pool.Len() != cap {
		t.Fatalf("pool grew to %d past its cap of %d", to.pool.Len(), cap)
	}
	// Over-cap txs are dropped without penalising the relayer: a full pool is our
	// limit, not the sender's misbehaviour, so honest relayers must not accumulate
	// a ban for it.
	snap := to.tg.Snapshot()
	if got := snap.Dropped; got != len(senders)-cap {
		t.Fatalf("dropped %d over-cap txs, want %d", got, len(senders)-cap)
	}
	if snap.Rejected != 0 {
		t.Fatalf("penalised the relayer %d times for a full pool, want 0", snap.Rejected)
	}
}
