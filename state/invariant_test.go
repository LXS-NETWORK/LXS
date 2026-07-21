package state

import (
	"math/big"
	"math/rand"
	"testing"

	"lxs/common"
	"lxs/contracts"
	"lxs/crypto"
	"lxs/types"
)

// Long random tx sequences that stress the state transition's core invariants:
// whether any sequence of valid transactions can violate conservation, replay
// protection, or determinism.

// bigTransfer builds a signed transfer with a big.Int value (the state_test
// `signed` helper only takes an int64).
func bigTransfer(t *testing.T, k *crypto.PrivateKey, nonce uint64, to common.Address, value *big.Int, gasLimit uint64) *types.Transaction {
	t.Helper()
	tx := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: &to,
		Value: value, GasLimit: gasLimit, GasPrice: big.NewInt(1),
	}
	if err := tx.Sign(k); err != nil {
		t.Fatal(err)
	}
	return tx
}

func totalBalance(s *State, accts ...common.Address) *big.Int {
	sum := new(big.Int)
	for _, a := range accts {
		sum.Add(sum, s.Balance(a))
	}
	return sum
}

// TestValueConservationAndDeterminism runs hundreds of random valid transfers and
// checks three invariants at once:
//
//   - Conservation: no LXS created or destroyed; every coin lost is gained by a
//     recipient or the coinbase as fee.
//   - Nonce monotonicity: each applied tx advances its sender's nonce by one.
//   - Determinism: replaying the same tx list on a fresh state yields the same
//     root (catches a root computation that forgot to sort).
func TestValueConservationAndDeterminism(t *testing.T) {
	r := rand.New(rand.NewSource(7))

	const n = 8
	keys := make([]*crypto.PrivateKey, n)
	addrs := make([]common.Address, n)
	fund := common.LXS(1_000_000)

	build := func() *State {
		st := New()
		for i := range addrs {
			st.Credit(addrs[i], fund)
		}
		return st
	}
	for i := range keys {
		keys[i] = key(t)
		addrs[i] = keys[i].Address()
	}
	coinbase := key(t).Address()
	total := new(big.Int).Mul(fund, big.NewInt(n)) // coinbase starts at 0

	s := build()
	nonces := make([]uint64, n)
	var txs []*types.Transaction

	const rounds = 600
	applied := 0
	for i := 0; i < rounds; i++ {
		si := r.Intn(n)
		bal := new(big.Int).Set(s.Balance(addrs[si]))
		maxFee := new(big.Int).SetUint64(types.IntrinsicGas) // gasPrice 1
		if bal.Cmp(maxFee) <= 0 {
			continue // cannot even afford the fee
		}
		room := new(big.Int).Sub(bal, maxFee)
		value := new(big.Int).Rand(r, new(big.Int).Add(room, big.NewInt(1))) // [0, room]
		to := addrs[r.Intn(n)]

		tx := bigTransfer(t, keys[si], nonces[si], to, value, types.IntrinsicGas)
		if _, _, _, err := ApplyTx(s, tx, coinbase, 30_000_000); err != nil {
			t.Fatalf("a tx built to be valid was rejected: %v", err)
		}
		nonces[si]++
		txs = append(txs, tx)
		applied++
	}

	// Conservation: accounts + coinbase + burned must sum to the initial supply.
	// A slice of every fee is burned, so the burned total is part of the ledger.
	got := totalBalance(s, append(append([]common.Address{}, addrs...), coinbase)...)
	got.Add(got, s.Burned())
	if got.Cmp(total) != 0 {
		t.Fatalf("value not conserved after %d txs: got %s, want %s (delta %s)",
			applied, got, total, new(big.Int).Sub(got, total))
	}

	// Nonces advanced exactly as many times as each account sent.
	for i := range addrs {
		if s.Nonce(addrs[i]) != nonces[i] {
			t.Fatalf("account %d nonce = %d, want %d", i, s.Nonce(addrs[i]), nonces[i])
		}
	}

	// Determinism: the same txs on a fresh identical state reach the same root.
	s2 := build()
	for _, tx := range txs {
		if _, _, _, err := ApplyTx(s2, tx, coinbase, 30_000_000); err != nil {
			t.Fatalf("replay rejected a tx: %v", err)
		}
	}
	if s.Root() != s2.Root() {
		t.Fatalf("state transition is NON-DETERMINISTIC: %s != %s", s.Root().Hex(), s2.Root().Hex())
	}
	t.Logf("applied %d/%d transfers, conserved and deterministic", applied, rounds)
}

// TestContractTxsConserveAndAreAtomic mixes ERC-20 deploys and transfers,
// including reverting ones. A reverted call must stay atomic (gas paid, nonce
// advanced, other writes rolled back) and value must be conserved throughout.
func TestContractTxsConserveAndAreAtomic(t *testing.T) {
	r := rand.New(rand.NewSource(11))

	dev := key(t)
	s := New()
	fund := common.LXS(10_000_000)
	s.Credit(dev.Address(), fund)
	coinbase := key(t).Address()
	total := new(big.Int).Set(fund) // coinbase 0, contract 0

	// Deploy the ERC-20 (nonce 0).
	supply := common.LXS(1_000_000)
	deploy := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID, Nonce: 0, To: nil,
		Value: big.NewInt(0), GasLimit: 5_000_000, GasPrice: big.NewInt(1), Data: contracts.ERC20Init(supply),
	}
	if err := deploy.Sign(dev); err != nil {
		t.Fatal(err)
	}
	if _, st, _, err := ApplyTx(s, deploy, coinbase, 30_000_000); err != nil || st != types.ReceiptSuccess {
		t.Fatalf("deploy failed: st=%d err=%v", st, err)
	}
	token := CreateAddress(dev.Address(), 0)

	nonce := uint64(1)
	reverts, oks := 0, 0
	for i := 0; i < 200; i++ {
		bob := key(t).Address()
		// Overspend the token about half the time to force reverts, which must
		// still be atomic and conserving.
		amount := common.LXS(int64(r.Intn(2_000_000))) // may exceed token supply
		call := &types.Transaction{
			Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: &token,
			Value: big.NewInt(0), GasLimit: 500_000, GasPrice: big.NewInt(1),
			Data: contracts.TransferCalldata(bob, amount),
		}
		if err := call.Sign(dev); err != nil {
			t.Fatal(err)
		}
		_, st, _, err := ApplyTx(s, call, coinbase, 30_000_000)
		if err != nil {
			t.Fatalf("a contract call must not error the block: %v", err)
		}
		if st == types.ReceiptSuccess {
			oks++
		} else {
			reverts++
		}
		nonce++
	}

	// Native value is conserved regardless of reverts: these calls move no native
	// value, only gas -> coinbase (minus the fee burn the burned total accounts for).
	got := totalBalance(s, dev.Address(), coinbase)
	got.Add(got, s.Burned())
	if got.Cmp(total) != 0 {
		t.Fatalf("native value not conserved: got %s, want %s", got, total)
	}
	// Nonce advanced once per included tx (deploy + 200 calls), revert or not.
	if s.Nonce(dev.Address()) != nonce {
		t.Fatalf("nonce = %d, want %d — a reverted call must still advance it", s.Nonce(dev.Address()), nonce)
	}
	if oks == 0 || reverts == 0 {
		t.Fatalf("test is not exercising both paths: %d ok, %d reverts", oks, reverts)
	}
	t.Logf("%d successful transfers, %d reverted — all atomic and conserving", oks, reverts)
}
