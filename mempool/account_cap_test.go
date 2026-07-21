package mempool

import (
	"testing"

	"lxs/state"
)

// One account must not be able to monopolise the pool. Without a per-account cap, a
// single zero-balance, zero-fee sender queues thousands of future-nonce txs for
// free, starving honest txs and freezing block production.
func TestPoolResistsSingleAccountFlood(t *testing.T) {
	m := New(100_000) // global cap far above the per-account cap, so the account cap is what bites
	k := mustKey(t)

	accepted, rejected := 0, 0
	for n := uint64(1); n <= maxPerAccount+50; n++ {
		if err := m.Add(txFrom(t, k, n, 0, 0), testChainID); err == nil { // 0 value, 0 gas price
			accepted++
		} else {
			rejected++
		}
	}
	if accepted > maxPerAccount {
		t.Fatalf("one account queued %d txs; the per-account cap is %d — pool can be monopolised", accepted, maxPerAccount)
	}
	if rejected == 0 {
		t.Fatal("no tx was rejected — the per-account cap is not enforced")
	}
}

// On a full pool a higher-fee tx must displace the lowest-fee one; a tx that cannot
// outbid the cheapest resident is rejected. Otherwise a full pool shuts out paying
// users behind low-fee backlog.
func TestFullPoolEvictsLowestFeeForHigher(t *testing.T) {
	m := New(3)
	for i := 0; i < 3; i++ {
		if err := m.Add(txFrom(t, mustKey(t), 0, 0, 1), testChainID); err != nil {
			t.Fatal(err)
		}
	}
	hi := txFrom(t, mustKey(t), 0, 0, 5)
	if err := m.Add(hi, testChainID); err != nil {
		t.Fatalf("a higher-fee tx must displace a low-fee one on a full pool: %v", err)
	}
	if _, ok := m.Get(hi.Hash()); !ok {
		t.Fatal("the higher-fee tx was not admitted after eviction")
	}
	lo := txFrom(t, mustKey(t), 0, 0, 1)
	if err := m.Add(lo, testChainID); err != ErrPoolFull {
		t.Fatalf("a non-outbidding tx on a full pool must be ErrPoolFull, got %v", err)
	}
}

// Demote drops txs whose nonce is below the committed head nonce (e.g. a competing
// same-nonce tx was mined), which Pending skips but would otherwise occupy slots.
func TestDemoteDropsStaleNonces(t *testing.T) {
	m := New(100)
	s := state.New()
	k := mustKey(t)
	// Queue nonces 0..3.
	for n := uint64(0); n <= 3; n++ {
		if err := m.Add(txFrom(t, k, n, 0, 1), testChainID); err != nil {
			t.Fatal(err)
		}
	}
	// The account's committed nonce advances to 2 (0 and 1 were mined elsewhere).
	s.SetNonce(k.Address(), 2)
	if dropped := m.Demote(s); dropped != 2 {
		t.Fatalf("Demote dropped %d, want 2 (nonces 0 and 1 are now stale)", dropped)
	}
	if m.Len() != 2 {
		t.Fatalf("pool holds %d after demote, want 2 (nonces 2,3)", m.Len())
	}
}
