package node

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
)

const faucetChainID = 1337

// newFaucetHarness builds a real blockchain whose only funded account is the
// faucet wallet, so the faucet can actually dispense. dispense is the per-claim
// amount; fund is the faucet wallet's starting balance (the hard ceiling).
func newFaucetHarness(t *testing.T, dispense, fund *big.Int) (*crypto.PrivateKey, *core.Blockchain, *mempool.Mempool, *Faucet) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	g := &core.Genesis{
		ChainID:   faucetChainID,
		Timestamp: 1_700_000_000_000,
		GasLimit:  10_000_000,
		Alloc:     map[common.Address]*core.BigStr{key.Address(): {Int: new(big.Int).Set(fund)}},
	}
	bc := core.NewMemBlockchain(g)
	pool := mempool.New(1000)
	f := NewFaucet(key, dispense, bc, pool, nil)
	return key, bc, pool, f
}

// addrHex fabricates a distinct, valid address for claim N.
func addrHex(i int) string {
	var a common.Address
	a[19] = byte(i)
	a[18] = byte(i >> 8)
	a[17] = byte(i >> 16)
	// non-zero high byte so it never collides with the zero address
	a[0] = 0xAA
	return a.Hex()
}

func postFaucet(h http.Handler, ip, addr string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"address": addr})
	r := httptest.NewRequest(http.MethodPost, "/faucet", bytes.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A new address gets funded exactly once; a second claim for the same address is
// refused. Without this the faucet is a free tap that one address drains forever.
func TestFaucetDispensesThenRejectsSecondClaim(t *testing.T) {
	_, _, pool, f := newFaucetHarness(t, big.NewInt(1000), common.LXS(1))
	addr := addrHex(1)

	if w := postFaucet(f, "1.2.3.4", addr); w.Code != http.StatusOK {
		t.Fatalf("first claim: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if pool.Len() != 1 {
		t.Fatalf("first claim did not enter the mempool: pool len %d", pool.Len())
	}
	if w := postFaucet(f, "1.2.3.4", addr); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second claim: got %d, want 429", w.Code)
	}
	if pool.Len() != 1 {
		t.Fatalf("second claim dispensed anyway: pool len %d", pool.Len())
	}
}

// An empty faucet wallet must fail loud (503), never silently pretend to
// dispense. The operator's cue to top it up.
func TestFaucetEmptyWalletFailsLoud(t *testing.T) {
	// fund far below one claim's gas + value, so CheckState rejects.
	_, _, pool, f := newFaucetHarness(t, big.NewInt(1000), big.NewInt(500))
	w := postFaucet(f, "1.2.3.4", addrHex(1))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty wallet: got %d, want 503", w.Code)
	}
	if pool.Len() != 0 {
		t.Fatalf("empty faucet still enqueued a tx: pool len %d", pool.Len())
	}
}

// The claimed set is bounded: past maxClaims it evicts oldest-first instead of
// growing without limit. An unbounded map keyed by attacker-supplied addresses is
// itself the memory-exhaustion DoS the faucet must not open.
func TestFaucetBoundedClaimedSetEvictsOldest(t *testing.T) {
	_, _, _, f := newFaucetHarness(t, big.NewInt(1000), common.LXS(1_000_000))
	f.maxClaims = 4

	for i := 1; i <= 10; i++ { // 10 distinct addresses through a size-4 set
		if w := postFaucet(f, "10.0.0.1", addrHex(i)); w.Code != http.StatusOK {
			t.Fatalf("claim %d: got %d, want 200 (%s)", i, w.Code, w.Body.String())
		}
	}
	if len(f.claimed) != 4 {
		t.Fatalf("claimed set not bounded: size %d, want 4", len(f.claimed))
	}
	// The oldest address (#1) was evicted, so it may claim again (proves eviction,
	// not just a cap that stopped inserting).
	if w := postFaucet(f, "10.0.0.1", addrHex(1)); w.Code != http.StatusOK {
		t.Fatalf("evicted address re-claim: got %d, want 200 — eviction did not happen", w.Code)
	}
	// A still-remembered recent address (#10) is still refused.
	if w := postFaucet(f, "10.0.0.1", addrHex(10)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("recent address: got %d, want 429 — it was wrongly evicted", w.Code)
	}
}

// /faucet must sit behind a per-IP rate limiter in the real node.New wiring, not
// exposed unthrottled. One IP firing many claims (distinct addresses, so the
// claim-once rule does not stop them) is capped to the burst.
func TestFaucetIsRateLimitedInNode(t *testing.T) {
	key, bc, pool, _ := newFaucetHarness(t, big.NewInt(1000), common.LXS(1_000_000))
	n := New(Config{RPCAddr: "127.0.0.1:0", FaucetKey: key, FaucetAmount: big.NewInt(1000)}, bc, pool, nil)
	h := n.srv.Handler // the real outer mux: rate-limiter -> faucet

	ok, limited := 0, 0
	for i := 1; i <= 20; i++ { // distinct addresses, same IP, back-to-back
		w := postFaucet(h, "9.9.9.9", addrHex(i))
		switch w.Code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("claim %d: unexpected %d (%s)", i, w.Code, w.Body.String())
		}
	}
	if ok > FaucetRateLimit.Burst {
		t.Fatalf("rate limiter did not cap the flood: %d passed, burst is %d", ok, FaucetRateLimit.Burst)
	}
	if limited == 0 {
		t.Fatalf("no request was rate-limited — /faucet is NOT behind the limiter (the audit HIGH)")
	}
	if pool.Len() != ok {
		t.Fatalf("mempool holds %d txs but %d claims succeeded", pool.Len(), ok)
	}
}
