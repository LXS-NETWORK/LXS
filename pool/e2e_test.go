package pool

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"lxs/crypto"
)

// TestWorkerFetchAndParse proves the worker parses real served work — the wire
// format both binaries must agree on.
func TestWorkerFetchAndParse(t *testing.T) {
	srv, bc, _, _ := newTestPool(t, 4_000_000, Config{MinShareDiff: 1})
	srv.tick()
	web := httptest.NewServer(srv.Handler())
	defer web.Close()

	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{BaseURL: web.URL, Coinbase: k.Address().Hex(), Logf: func(string, ...any) {}}
	got, err := w.fetchWork(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkID == "" || got.Height != bc.Head().Height()+1 || got.ShareDiff == 0 {
		t.Fatalf("bad work: %+v", got)
	}
}

// TestPoolEndToEnd is the whole promise in one test: two workers with nothing
// but a payout address and the pool's URL join over real HTTP, the pool wins
// blocks from their shares, rewards mature, and BOTH workers end up with LXS in
// their on-chain balance — paid automatically, split by contributed work. If any
// stage (work serving, share validation, PPLNS, maturity, payout txs, inclusion
// of those txs in later blocks) breaks, the balances never appear.
func TestPoolEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-second integration test")
	}
	// Difficulty 6000 with divisor 64 → a share every ~94 hashes: both workers
	// log plenty of shares between blocks, so both sit in the PPLNS window;
	// blocks still take only ~6000 hashes.
	srv, bc, _, poolKey := newTestPool(t, 6_000, Config{
		MinShareDiff:  1,
		Confirmations: 3,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go srv.Run(ctx)

	web := httptest.NewServer(srv.Handler())
	defer web.Close()

	wa, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	wb, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []*crypto.PrivateKey{wa, wb} {
		w := &Worker{BaseURL: web.URL, Coinbase: k.Address().Hex(), Logf: func(string, ...any) {}}
		go func() { _ = w.Run(ctx) }()
	}

	// Success: both workers hold on-chain LXS. Every stage of the pipeline must
	// have fired for that to be true.
	deadline := time.After(55 * time.Second)
	for bc.BalanceAt(wa.Address()).Sign() == 0 || bc.BalanceAt(wb.Address()).Sign() == 0 {
		select {
		case <-deadline:
			srv.mu.Lock()
			shares, blocks, pend, owed := srv.totalShares, srv.totalBlocks, len(srv.pending), len(srv.balances)
			srv.mu.Unlock()
			t.Fatalf("workers unpaid after 55s: height=%d shares=%d blocks=%d pending=%d owed=%d a=%s b=%s",
				bc.Head().Height(), shares, blocks, pend, owed,
				bc.BalanceAt(wa.Address()), bc.BalanceAt(wb.Address()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.totalBlocks == 0 || srv.totalShares == 0 {
		t.Fatal("paid without recorded work — accounting is broken")
	}
	t.Logf("E2E: height=%d shares=%d blocks=%d paidA=%s paidB=%s poolWallet=%s",
		bc.Head().Height(), srv.totalShares, srv.totalBlocks,
		bc.BalanceAt(wa.Address()), bc.BalanceAt(wb.Address()), bc.BalanceAt(poolKey.Address()))
}
