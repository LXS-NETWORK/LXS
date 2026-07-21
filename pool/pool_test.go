package pool

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

const testChainID = 777

// newTestPool builds a real in-memory chain + pool server. genesisDiff picks how
// hard blocks are: tests that must FIND blocks keep it small; tests about share
// bookkeeping make it large so a share is never accidentally a block.
func newTestPool(t *testing.T, genesisDiff uint64, cfg Config) (*Server, *core.Blockchain, *mempool.Mempool, *crypto.PrivateKey) {
	t.Helper()
	g := &core.Genesis{
		ChainID:    testChainID,
		Timestamp:  1_700_000_000_000,
		GasLimit:   10_000_000,
		Difficulty: genesisDiff,
	}
	bc := core.NewMemBlockchain(g)
	mp := mempool.New(1000)
	core.BindMempool(bc, mp)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	prod := core.NewProducer(bc, mp, key.Address())
	srv, err := NewServer(cfg, bc, prod, key, mp, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return srv, bc, mp, key
}

func addr(b byte) common.Address {
	var a common.Address
	a[19] = b
	return a
}

// grindToTarget finds a nonce meeting target but NOT belowTarget (pass nil to
// accept any). Lets tests manufacture a share that is definitely not a block.
func grindToTarget(t *testing.T, hdr *types.Header, target, notBelow *big.Int) uint64 {
	t.Helper()
	var clone types.Header
	b := mustJSON(hdr)
	if err := json.Unmarshal(b, &clone); err != nil {
		t.Fatal(err)
	}
	for nonce := uint64(0); nonce < 50_000_000; nonce++ {
		clone.Nonce = nonce
		clone.InvalidateHash()
		h := clone.Hash()
		v := new(big.Int).SetBytes(h[:])
		if v.Cmp(target) <= 0 {
			if notBelow != nil && v.Cmp(notBelow) <= 0 {
				continue // too good: it would be a block, keep looking
			}
			return nonce
		}
	}
	t.Fatal("no nonce found in bounded search — genesisDiff too high for this test")
	return 0
}

func TestWorkTemplateMatchesChain(t *testing.T) {
	srv, bc, _, key := newTestPool(t, 4_000_000, Config{MinShareDiff: 1})
	srv.tick()

	srv.mu.Lock()
	tmpl := srv.templates[srv.current]
	srv.mu.Unlock()
	if tmpl == nil {
		t.Fatal("no template after tick")
	}
	if tmpl.hdr.ParentHash != bc.Head().Hash() {
		t.Fatal("template does not build on the head")
	}
	if tmpl.hdr.Proposer != key.Address() {
		t.Fatal("template proposer is not the pool wallet — rewards would be unspendable by the payout engine")
	}
	if tmpl.shareDiff != 4_000_000/64 {
		t.Fatalf("share difficulty = %d, want blockDiff/64", tmpl.shareDiff)
	}
}

func TestShareAcceptRejectAndDup(t *testing.T) {
	// Block difficulty high enough that a share is essentially never a block.
	srv, _, _, _ := newTestPool(t, 40_000_000, Config{MinShareDiff: 1})
	srv.tick()
	srv.mu.Lock()
	tmpl := srv.templates[srv.current]
	srv.mu.Unlock()

	nonce := grindToTarget(t, tmpl.hdr, tmpl.shareTarget, tmpl.blockTarget)
	worker := addr(1)

	if isBlock, err := srv.handleShare(tmpl.id, nonce, worker); err != nil || isBlock {
		t.Fatalf("valid share rejected: block=%v err=%v", isBlock, err)
	}
	if _, err := srv.handleShare(tmpl.id, nonce, worker); err != errDuplicate {
		t.Fatalf("duplicate share must be rejected, got %v", err)
	}
	// A nonce that fails the share target must earn nothing. Find one: hash above
	// shareTarget (i.e. reject candidate) — try a few nonces until one fails.
	bad := uint64(0)
	for {
		var clone types.Header
		if err := json.Unmarshal(mustJSON(tmpl.hdr), &clone); err != nil {
			t.Fatal(err)
		}
		clone.Nonce = bad
		clone.InvalidateHash()
		h := clone.Hash()
		if new(big.Int).SetBytes(h[:]).Cmp(tmpl.shareTarget) > 0 {
			break
		}
		bad++
	}
	if _, err := srv.handleShare(tmpl.id, bad, worker); err != errBadShare {
		t.Fatalf("bad share must be rejected, got %v", err)
	}
	if _, err := srv.handleShare(common.Hash{0xde, 0xad}, nonce, worker); err != errStale {
		t.Fatalf("unknown work must be stale, got %v", err)
	}
	if got := srv.totalShares; got != 1 {
		t.Fatalf("exactly one share should have been credited, got %d", got)
	}
}

func TestShareMeetingBlockTargetCommitsBlock(t *testing.T) {
	srv, bc, _, key := newTestPool(t, 2_000, Config{MinShareDiff: 1})
	srv.tick()
	srv.mu.Lock()
	tmpl := srv.templates[srv.current]
	srv.mu.Unlock()

	before := bc.Head().Height()
	nonce := grindToTarget(t, tmpl.hdr, tmpl.blockTarget, nil) // meets the BLOCK target
	isBlock, err := srv.handleShare(tmpl.id, nonce, addr(2))
	if err != nil || !isBlock {
		t.Fatalf("block-quality share: block=%v err=%v", isBlock, err)
	}
	if bc.Head().Height() != before+1 {
		t.Fatal("chain did not advance — the found block was not committed")
	}
	if bc.Head().Header.Proposer != key.Address() {
		t.Fatal("committed block does not pay the pool wallet")
	}
	if len(srv.pending) != 1 {
		t.Fatalf("found block must be pending maturity, got %d", len(srv.pending))
	}
	if pay, ok := srv.pending[0].Payouts[addr(2)]; !ok || pay.Sign() <= 0 {
		t.Fatal("the worker who contributed the share got no payout snapshot")
	}
}

func TestPPLNSProportionalSplit(t *testing.T) {
	srv, _, _, _ := newTestPool(t, 40_000_000, Config{MinShareDiff: 1, WindowFactor: 2})
	// 3 shares from A, 1 from B, all diff 1000 → A gets 75%, B 25%.
	for i := 0; i < 3; i++ {
		srv.ring = append(srv.ring, share{addr: addr(0xA), diff: 1000})
	}
	srv.ring = append(srv.ring, share{addr: addr(0xB), diff: 1000})

	pot := big.NewInt(1_000_000)
	out := srv.pplnsLocked(pot, 2000) // window = 2×2000 = 4000 → all 4 shares in
	if got := out[addr(0xA)].Int64(); got != 750_000 {
		t.Fatalf("A share = %d, want 750000", got)
	}
	if got := out[addr(0xB)].Int64(); got != 250_000 {
		t.Fatalf("B share = %d, want 250000", got)
	}
}

func TestPPLNSWindowExcludesOldShares(t *testing.T) {
	srv, _, _, _ := newTestPool(t, 40_000_000, Config{MinShareDiff: 1, WindowFactor: 1})
	// Old work by C, then a full window of A. Window = 1×4000 = 4 shares of
	// diff 1000 → only A's latest 4 count; C must get nothing.
	srv.ring = append(srv.ring, share{addr: addr(0xC), diff: 1000})
	for i := 0; i < 4; i++ {
		srv.ring = append(srv.ring, share{addr: addr(0xA), diff: 1000})
	}
	out := srv.pplnsLocked(big.NewInt(1_000_000), 4000)
	if _, ok := out[addr(0xC)]; ok {
		t.Fatal("share outside the PPLNS window was paid — pool-hopping would profit")
	}
	if got := out[addr(0xA)].Int64(); got != 1_000_000 {
		t.Fatalf("A must take the whole pot, got %d", got)
	}
}

func TestMatureCreditsThenPaysOut(t *testing.T) {
	srv, bc, mp, key := newTestPool(t, 1, Config{MinShareDiff: 1, Confirmations: 3})
	srv.tick()
	srv.mu.Lock()
	tmpl := srv.templates[srv.current]
	srv.mu.Unlock()

	// Worker wins a block (difficulty 1: nonce 0 works).
	nonce := grindToTarget(t, tmpl.hdr, tmpl.blockTarget, nil)
	if isBlock, err := srv.handleShare(tmpl.id, nonce, addr(7)); err != nil || !isBlock {
		t.Fatalf("expected a block, got block=%v err=%v", isBlock, err)
	}
	won := srv.pending[0]

	// Advance the chain past maturity, then tick: the credit must appear, then
	// the payout tx must land in the mempool paying the worker's full balance.
	prod := core.NewProducer(bc, mp, key.Address())
	for i := uint64(0); i < 3; i++ {
		if _, err := prod.Seal(); err != nil {
			t.Fatal(err)
		}
	}
	srv.tick()

	if len(srv.pending) != 0 {
		t.Fatal("matured block still pending")
	}
	if srv.balances[addr(7)] != nil && srv.balances[addr(7)].Sign() != 0 {
		t.Fatal("balance not swept by payout")
	}
	txs := mp.Pending(bc.StateSnapshot(), 10_000_000)
	if len(txs) != 1 {
		t.Fatalf("expected exactly the payout tx in the mempool, got %d", len(txs))
	}
	if txs[0].To == nil || *txs[0].To != addr(7) {
		t.Fatal("payout tx pays the wrong address")
	}
	if txs[0].Value.Cmp(won.Payouts[addr(7)]) != 0 {
		t.Fatalf("payout %s ≠ snapshot %s", txs[0].Value, won.Payouts[addr(7)])
	}
	// The pot must be reward+fees−reserve (no fees here), never more than the
	// reward the pool actually received.
	reward := state.BlockRewardAt(won.Height)
	if txs[0].Value.Cmp(reward) >= 0 {
		t.Fatal("payout exceeds the block reward — pool would go insolvent")
	}
}

func TestOrphanedBlockIsNotCredited(t *testing.T) {
	srv, bc, mp, key := newTestPool(t, 1, Config{MinShareDiff: 1, Confirmations: 2})
	// Fabricate a pending block that never became canonical.
	srv.pending = append(srv.pending, &foundBlock{
		Height:  1,
		Hash:    common.Hash{0xbb},
		Payouts: map[common.Address]*big.Int{addr(9): big.NewInt(1000)},
	})
	prod := core.NewProducer(bc, mp, key.Address())
	for i := 0; i < 4; i++ {
		if _, err := prod.Seal(); err != nil {
			t.Fatal(err)
		}
	}
	srv.tick()
	if len(srv.pending) != 0 {
		t.Fatal("orphaned block still pending")
	}
	if srv.balances[addr(9)] != nil {
		t.Fatal("orphaned reward was credited — the pool never received that money")
	}
	if srv.totalOrphans != 1 {
		t.Fatalf("orphan not counted, got %d", srv.totalOrphans)
	}
}

func TestLedgerPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	srv, bc, mp, key := newTestPool(t, 40_000_000, Config{MinShareDiff: 1, StatePath: path})
	srv.balances[addr(3)] = big.NewInt(123456)
	srv.pending = append(srv.pending, &foundBlock{
		Height: 9, Hash: common.Hash{0xcc},
		Payouts: map[common.Address]*big.Int{addr(4): big.NewInt(777)},
	})
	srv.totalShares, srv.totalBlocks = 42, 2
	srv.mu.Lock()
	srv.saveLocked()
	srv.mu.Unlock()

	prod := core.NewProducer(bc, mp, key.Address())
	srv2, err := NewServer(Config{MinShareDiff: 1, StatePath: path}, bc, prod, key, mp, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if srv2.balances[addr(3)].Int64() != 123456 {
		t.Fatal("balance lost across restart — worker money vanished")
	}
	if len(srv2.pending) != 1 || srv2.pending[0].Payouts[addr(4)].Int64() != 777 {
		t.Fatal("pending block lost across restart")
	}
	if srv2.totalShares != 42 || srv2.totalBlocks != 2 {
		t.Fatal("counters lost across restart")
	}
}

func TestCorruptLedgerFailsLoud(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &core.Genesis{ChainID: testChainID, Timestamp: 1_700_000_000_000, GasLimit: 10_000_000, Difficulty: 1}
	bc := core.NewMemBlockchain(g)
	mp := mempool.New(10)
	key, _ := crypto.GenerateKey()
	prod := core.NewProducer(bc, mp, key.Address())
	if _, err := NewServer(Config{StatePath: path}, bc, prod, key, mp, nil, t.Logf); err == nil {
		t.Fatal("corrupt ledger must refuse to start — silently zeroing balances is theft by accident")
	}
}
