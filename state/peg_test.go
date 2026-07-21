package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/contracts"
	"lxs/crypto"
	"lxs/types"
)

// The custodial LXS<->Base peg end to end on the VM: lock native LXS in the
// PegVault, operator mints wLXS, it transfers, the holder redeems (burns), the
// operator releases the locked LXS. Every step asserts the backing invariant
// reserve() >= wLXS.totalSupply().
func TestCustodialPegRoundTripStaysBacked(t *testing.T) {
	op := key(t) // the custodial operator (also plays the user here)
	s := New()
	s.Credit(op.Address(), common.LXS(100))

	apply := func(signer *crypto.PrivateKey, nonce uint64, to *common.Address, value *big.Int, data []byte) uint64 {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 3_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(signer); err != nil {
			t.Fatal(err)
		}
		_, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st
	}
	readU := func(to common.Address, data []byte) *big.Int {
		out, err := Call(s, op.Address(), to, data, 3_000_000)
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		return new(big.Int).SetBytes(out)
	}

	if st := apply(op, 0, nil, big.NewInt(0), contracts.PegVaultInit(op.Address())); st != types.ReceiptSuccess {
		t.Fatal("PegVault deploy failed")
	}
	vault := CreateAddress(op.Address(), 0)
	if st := apply(op, 1, nil, big.NewInt(0), contracts.WrappedLXSInit(op.Address())); st != types.ReceiptSuccess {
		t.Fatal("WrappedLXS deploy failed")
	}
	wlxs := CreateAddress(op.Address(), 1)

	inv := func(step string) {
		reserve := readU(vault, contracts.PegReserveCalldata())
		supply := readU(wlxs, contracts.TotalSupplyCalldata())
		if reserve.Cmp(supply) < 0 {
			t.Fatalf("%s: peg UNDER-BACKED — reserve %s < wLXS supply %s", step, reserve, supply)
		}
	}

	X := common.LXS(10)

	// 1) lock native LXS as backing
	if st := apply(op, 2, &vault, X, contracts.PegLockCalldata()); st != types.ReceiptSuccess {
		t.Fatal("lock failed")
	}
	if got := readU(vault, contracts.PegReserveCalldata()); got.Cmp(X) != 0 {
		t.Fatalf("reserve after lock = %s, want %s", got, X)
	}
	inv("after lock")

	// 2) operator mints the same amount of wLXS
	if st := apply(op, 3, &wlxs, big.NewInt(0), contracts.WlxsMintCalldata(big.NewInt(0), op.Address(), X)); st != types.ReceiptSuccess {
		t.Fatal("mint failed")
	}
	if got := readU(wlxs, contracts.BalanceOfCalldata(op.Address())); got.Cmp(X) != 0 {
		t.Fatalf("wLXS balance after mint = %s, want %s", got, X)
	}
	inv("after mint")

	// 3) redeem burns the wLXS; supply drops to 0
	if st := apply(op, 4, &wlxs, big.NewInt(0), contracts.WlxsRedeemCalldata(X)); st != types.ReceiptSuccess {
		t.Fatal("redeem failed")
	}
	if got := readU(wlxs, contracts.TotalSupplyCalldata()); got.Sign() != 0 {
		t.Fatalf("wLXS supply after redeem = %s, want 0", got)
	}
	inv("after redeem")

	// 4) operator releases the locked LXS back to the redeemer
	if st := apply(op, 5, &vault, big.NewInt(0), contracts.PegReleaseCalldata(big.NewInt(0), op.Address(), X)); st != types.ReceiptSuccess {
		t.Fatal("release failed")
	}
	if got := readU(vault, contracts.PegReserveCalldata()); got.Sign() != 0 {
		t.Fatalf("reserve after release = %s, want 0", got)
	}
	inv("after release")
}

// The peg is custodial, so the contracts must bound a compromised operator: only
// the operator may mint or release, and a release can never exceed the reserve.
func TestPegGuardsBoundACompromisedOperator(t *testing.T) {
	op := key(t)
	mal := key(t) // a non-operator attacker
	s := New()
	s.Credit(op.Address(), common.LXS(100))
	s.Credit(mal.Address(), common.LXS(100))

	apply := func(signer *crypto.PrivateKey, nonce uint64, to *common.Address, value *big.Int, data []byte) uint64 {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 3_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(signer); err != nil {
			t.Fatal(err)
		}
		_, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st
	}

	apply(op, 0, nil, big.NewInt(0), contracts.PegVaultInit(op.Address()))
	vault := CreateAddress(op.Address(), 0)
	apply(op, 1, nil, big.NewInt(0), contracts.WrappedLXSInit(op.Address()))
	wlxs := CreateAddress(op.Address(), 1)
	apply(op, 2, &vault, common.LXS(10), contracts.PegLockCalldata()) // reserve = 10

	// GUARD 1: a non-operator cannot mint wLXS (minting against nothing de-backs the peg).
	if st := apply(mal, 0, &wlxs, big.NewInt(0), contracts.WlxsMintCalldata(big.NewInt(0), mal.Address(), common.LXS(1_000_000))); st != types.ReceiptFailed {
		t.Fatal("SECURITY: a non-operator minted wLXS — the peg is de-backed")
	}
	// GUARD 2: even the operator cannot release more than the reserve.
	if st := apply(op, 3, &vault, big.NewInt(0), contracts.PegReleaseCalldata(big.NewInt(0), op.Address(), common.LXS(11))); st != types.ReceiptFailed {
		t.Fatal("SECURITY: released more than the reserve holds")
	}
	// GUARD 3: a non-operator cannot release at all.
	if st := apply(mal, 1, &vault, big.NewInt(0), contracts.PegReleaseCalldata(big.NewInt(0), mal.Address(), common.LXS(5))); st != types.ReceiptFailed {
		t.Fatal("SECURITY: a non-operator drained the vault")
	}
}

// On-chain idempotency (the ChainBridge pattern): minting the same lock nonce twice
// must fail, so a relayer restart or double-submit can never double-mint. This is the
// safety property that lets an untrusted/buggy relayer never create unbacked wLXS.
func TestPegNonceIdempotency(t *testing.T) {
	op := key(t)
	s := New()
	s.Credit(op.Address(), common.LXS(100))
	apply := func(nonce uint64, to *common.Address, data []byte) uint64 {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: big.NewInt(0), GasLimit: 3_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(op); err != nil {
			t.Fatal(err)
		}
		_, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st
	}
	apply(0, nil, contracts.WrappedLXSInit(op.Address()))
	wlxs := CreateAddress(op.Address(), 0)
	X := common.LXS(10)

	// First mint of lock-nonce 7 succeeds.
	if st := apply(1, &wlxs, contracts.WlxsMintCalldata(big.NewInt(7), op.Address(), X)); st != types.ReceiptSuccess {
		t.Fatal("first mint of a nonce should succeed")
	}
	// Re-minting the SAME nonce must fail — the relayer cannot double-mint.
	if st := apply(2, &wlxs, contracts.WlxsMintCalldata(big.NewInt(7), op.Address(), X)); st != types.ReceiptFailed {
		t.Fatal("SECURITY: re-minting the same lock nonce succeeded — a relayer could double-mint unbacked wLXS")
	}
	// Balance reflects exactly one mint, not two.
	out, err := Call(s, op.Address(), wlxs, contracts.BalanceOfCalldata(op.Address()), 3_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := new(big.Int).SetBytes(out); got.Cmp(X) != 0 {
		t.Fatalf("balance = %s after a duplicate mint, want exactly %s (one mint)", got, X)
	}
}

// ERC-1046 tokenURI: the operator installs the LXS logo/metadata data: URI once, wallets
// read it via tokenURI(). It must be operator-only and one-shot (frozen after first set),
// so the branding is fixed like the rest of the token.
func TestWlxsTokenURISetOnceByOperator(t *testing.T) {
	op := key(t)
	mal := key(t)
	s := New()
	s.Credit(op.Address(), common.LXS(100))
	s.Credit(mal.Address(), common.LXS(100))

	apply := func(signer *crypto.PrivateKey, nonce uint64, to *common.Address, data []byte) uint64 {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: big.NewInt(0), GasLimit: 3_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(signer); err != nil {
			t.Fatal(err)
		}
		_, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st
	}
	readStr := func(to common.Address, data []byte) string {
		out, err := Call(s, op.Address(), to, data, 3_000_000)
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		if len(out) < 64 {
			return ""
		}
		n := new(big.Int).SetBytes(out[32:64]).Int64()
		if int64(len(out)) < 64+n {
			return ""
		}
		return string(out[64 : 64+n])
	}

	apply(op, 0, nil, contracts.WrappedLXSInit(op.Address()))
	wlxs := CreateAddress(op.Address(), 0)

	uri := "data:application/json;base64,eyJuYW1lIjoiTFhTIn0="
	// a non-operator cannot set the branding
	if st := apply(mal, 0, &wlxs, contracts.WlxsSetTokenURICalldata(uri)); st != types.ReceiptFailed {
		t.Fatal("SECURITY: a non-operator set the token URI")
	}
	// operator sets it once
	if st := apply(op, 1, &wlxs, contracts.WlxsSetTokenURICalldata(uri)); st != types.ReceiptSuccess {
		t.Fatal("operator setTokenURI failed")
	}
	if got := readStr(wlxs, contracts.WlxsTokenURICalldata()); got != uri {
		t.Fatalf("tokenURI = %q, want %q", got, uri)
	}
	// frozen: a second set must fail, so branding cannot change under holders
	if st := apply(op, 2, &wlxs, contracts.WlxsSetTokenURICalldata("data:xxx")); st != types.ReceiptFailed {
		t.Fatal("SECURITY: token URI was changed after being frozen")
	}
	if got := readStr(wlxs, contracts.WlxsTokenURICalldata()); got != uri {
		t.Fatalf("tokenURI changed after freeze = %q", got)
	}
}
