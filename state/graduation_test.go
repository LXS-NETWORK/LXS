package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/contracts"
	"lxs/crypto"
	"lxs/types"
)

// Graduation end to end on the VM: a creator commits LXS (>= minLiquidity) plus a
// chunk of their coin to the GraduationVault; both lock as backing. Then, on the
// "Base" side, the operator mints the WrappedToken against the locked coin, it
// redeems (burns), and the operator unwinds both legs from the vault. Asserts the
// commitment gate, the locked-backing amounts, mint idempotency, and the
// bounded-operator release guards — the same trust model as the peg.
func TestGraduationRoundTripAndGuards(t *testing.T) {
	op := key(t)      // the custodial operator
	creator := key(t) // the coin's creator, who graduates it
	s := New()
	s.Credit(op.Address(), common.LXS(100))
	s.Credit(creator.Address(), common.LXS(100))

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

	// The creator deploys their coin (a fixed-supply ERC-20, whole supply to them).
	if st := apply(creator, 0, nil, big.NewInt(0), contracts.UserTokenDeploy("HITMAN", "HIT", common.LXS(1_000_000))); st != types.ReceiptSuccess {
		t.Fatal("coin deploy failed")
	}
	coin := CreateAddress(creator.Address(), 0)

	// The operator deploys the vault with a 1-LXS commitment gate.
	minLiq := common.LXS(1)
	if st := apply(op, 0, nil, big.NewInt(0), contracts.GraduationVaultInit(op.Address(), minLiq)); st != types.ReceiptSuccess {
		t.Fatal("GraduationVault deploy failed")
	}
	vault := CreateAddress(op.Address(), 0)

	tokenAmt := common.LXS(1000) // coin committed to the pool
	lxsAmt := common.LXS(2)      // LXS committed (above the 1-LXS gate)

	// creator approves the vault to pull the committed coin FIRST, so the gate test that
	// follows can only revert for one reason: the min-liquidity check itself.
	if st := apply(creator, 1, &coin, big.NewInt(0), contracts.ApproveCalldata(vault, tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("approve failed")
	}
	// GATE: with the approval already in place and the coin available, a graduation below
	// the minimum liquidity must STILL revert — proving the gate, not a missing allowance,
	// is what rejects it. (A removed gate would let this succeed and fail the test.)
	if st := apply(creator, 2, &vault, new(big.Int).Sub(minLiq, big.NewInt(1)), contracts.GraduateCalldata(coin, tokenAmt)); st != types.ReceiptFailed {
		t.Fatal("SECURITY: a graduation below minLiquidity was accepted")
	}
	// the real graduation (above the gate) locks both legs.
	if st := apply(creator, 3, &vault, lxsAmt, contracts.GraduateCalldata(coin, tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("graduate failed")
	}
	if got := readU(vault, contracts.GradLxsReserveCalldata()); got.Cmp(lxsAmt) != 0 {
		t.Fatalf("locked LXS = %s, want %s", got, lxsAmt)
	}
	if got := readU(vault, contracts.GradTokenReserveCalldata(coin)); got.Cmp(tokenAmt) != 0 {
		t.Fatalf("locked coin = %s, want %s", got, tokenAmt)
	}

	// --- Base side: operator mints the WrappedToken against the locked coin ---
	if st := apply(op, 1, nil, big.NewInt(0), contracts.WrappedTokenInit(op.Address(), "HITMAN", "HIT")); st != types.ReceiptSuccess {
		t.Fatal("WrappedToken deploy failed")
	}
	wtok := CreateAddress(op.Address(), 1)
	// The constructor string encoding must round-trip, or wallets/DEXs show the wrong name.
	if got := readStr(wtok, []byte{0x06, 0xfd, 0xde, 0x03}); got != "HITMAN" { // name()
		t.Fatalf("WrappedToken name = %q, want HITMAN", got)
	}
	if got := readStr(wtok, []byte{0x95, 0xd8, 0x9b, 0x41}); got != "HIT" { // symbol()
		t.Fatalf("WrappedToken symbol = %q, want HIT", got)
	}

	// mint against the locked coin, keyed by the graduation nonce (0).
	if st := apply(op, 2, &wtok, big.NewInt(0), contracts.WrappedTokenMintCalldata(big.NewInt(0), op.Address(), tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("WrappedToken mint failed")
	}
	if got := readU(wtok, contracts.WrappedTokenBalanceCalldata(op.Address())); got.Cmp(tokenAmt) != 0 {
		t.Fatalf("wToken balance after mint = %s, want %s", got, tokenAmt)
	}
	// IDEMPOTENCY: re-minting the same graduation nonce must fail — no double-mint on restart.
	if st := apply(op, 3, &wtok, big.NewInt(0), contracts.WrappedTokenMintCalldata(big.NewInt(0), op.Address(), tokenAmt)); st != types.ReceiptFailed {
		t.Fatal("SECURITY: re-minting the same graduation nonce succeeded — a relayer could double-mint")
	}
	if got := readU(wtok, contracts.TotalSupplyCalldata()); got.Cmp(tokenAmt) != 0 {
		t.Fatalf("wToken supply = %s after a duplicate mint, want exactly %s", got, tokenAmt)
	}
	// redeem burns it back down.
	if st := apply(op, 4, &wtok, big.NewInt(0), contracts.WrappedTokenRedeemCalldata(tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("WrappedToken redeem failed")
	}
	if got := readU(wtok, contracts.TotalSupplyCalldata()); got.Sign() != 0 {
		t.Fatalf("wToken supply after redeem = %s, want 0", got)
	}

	// --- unwind guards on the vault (bounded operator, same as the peg) ---
	// a non-operator cannot release the locked LXS.
	if st := apply(creator, 4, &vault, big.NewInt(0), contracts.GradReleaseLxsCalldata(big.NewInt(0), creator.Address(), lxsAmt)); st != types.ReceiptFailed {
		t.Fatal("SECURITY: a non-operator released locked LXS")
	}
	// even the operator cannot release more LXS than is locked.
	if st := apply(op, 5, &vault, big.NewInt(0), contracts.GradReleaseLxsCalldata(big.NewInt(0), op.Address(), common.LXS(3))); st != types.ReceiptFailed {
		t.Fatal("SECURITY: released more LXS than the vault holds")
	}
	// the operator unwinds both legs.
	if st := apply(op, 6, &vault, big.NewInt(0), contracts.GradReleaseLxsCalldata(big.NewInt(0), creator.Address(), lxsAmt)); st != types.ReceiptSuccess {
		t.Fatal("releaseLxs failed")
	}
	if st := apply(op, 7, &vault, big.NewInt(0), contracts.GradReleaseTokenCalldata(big.NewInt(0), coin, creator.Address(), tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("releaseToken failed")
	}
	if got := readU(vault, contracts.GradLxsReserveCalldata()); got.Sign() != 0 {
		t.Fatalf("locked LXS after unwind = %s, want 0", got)
	}
	if got := readU(vault, contracts.GradTokenReserveCalldata(coin)); got.Sign() != 0 {
		t.Fatalf("locked coin after unwind = %s, want 0", got)
	}
	// NONCE-ONCE: releasing the same LXS nonce again must fail.
	if st := apply(op, 8, &vault, big.NewInt(0), contracts.GradReleaseLxsCalldata(big.NewInt(0), creator.Address(), lxsAmt)); st != types.ReceiptFailed {
		t.Fatal("SECURITY: released the same graduation LXS nonce twice")
	}
}

// The full graduation-to-pool path the operator's orchestrator drives: after a coin is
// graduated (LXS + coin locked), the operator mints wMeme against the locked coin and
// wLXS against the locked LXS, approves the router, and seeds a wMeme/wLXS pool. Uses a
// mock router that pulls both tokens exactly as UniswapV2Router02 does. Asserts the pool
// ends up holding BOTH legs and that each wrapped supply stays within its locked backing.
func TestGraduationSeedsPool(t *testing.T) {
	op := key(t)
	creator := key(t)
	s := New()
	s.Credit(op.Address(), common.LXS(100))
	s.Credit(creator.Address(), common.LXS(100))

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

	tokenAmt := common.LXS(1000)
	lxsAmt := common.LXS(5)

	// creator: deploy coin, approve, graduate.
	if st := apply(creator, 0, nil, big.NewInt(0), contracts.UserTokenDeploy("HITMAN", "HIT", common.LXS(1_000_000))); st != types.ReceiptSuccess {
		t.Fatal("coin deploy failed")
	}
	coin := CreateAddress(creator.Address(), 0)
	if st := apply(op, 0, nil, big.NewInt(0), contracts.GraduationVaultInit(op.Address(), common.LXS(1))); st != types.ReceiptSuccess {
		t.Fatal("vault deploy failed")
	}
	vault := CreateAddress(op.Address(), 0)
	if st := apply(creator, 1, &coin, big.NewInt(0), contracts.ApproveCalldata(vault, tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("approve vault failed")
	}
	if st := apply(creator, 2, &vault, lxsAmt, contracts.GraduateCalldata(coin, tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("graduate failed")
	}

	// operator: deploy router + both wrapped tokens.
	if st := apply(op, 1, nil, big.NewInt(0), contracts.MockRouterV2Init()); st != types.ReceiptSuccess {
		t.Fatal("router deploy failed")
	}
	router := CreateAddress(op.Address(), 1)
	if st := apply(op, 2, nil, big.NewInt(0), contracts.WrappedTokenInit(op.Address(), "HITMAN", "HIT")); st != types.ReceiptSuccess {
		t.Fatal("wMeme deploy failed")
	}
	wmeme := CreateAddress(op.Address(), 2)
	if st := apply(op, 3, nil, big.NewInt(0), contracts.WrappedLXSInit(op.Address())); st != types.ReceiptSuccess {
		t.Fatal("wLXS deploy failed")
	}
	wlxs := CreateAddress(op.Address(), 3)

	// operator: mint each wrapped leg against the locked backing (graduation nonce 0).
	if st := apply(op, 4, &wmeme, big.NewInt(0), contracts.WrappedTokenMintCalldata(big.NewInt(0), op.Address(), tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("mint wMeme failed")
	}
	if st := apply(op, 5, &wlxs, big.NewInt(0), contracts.WlxsMintCalldata(big.NewInt(0), op.Address(), lxsAmt)); st != types.ReceiptSuccess {
		t.Fatal("mint wLXS failed")
	}

	// each wrapped supply must be within what the vault actually locks (backed, not fake).
	if sup, res := readU(wmeme, contracts.TotalSupplyCalldata()), readU(vault, contracts.GradTokenReserveCalldata(coin)); sup.Cmp(res) > 0 {
		t.Fatalf("wMeme supply %s exceeds locked coin %s — unbacked", sup, res)
	}
	if sup, res := readU(wlxs, contracts.TotalSupplyCalldata()), readU(vault, contracts.GradLxsReserveCalldata()); sup.Cmp(res) > 0 {
		t.Fatalf("wLXS supply %s exceeds locked LXS %s — unbacked", sup, res)
	}

	// operator: approve the router for both, then seed the pool.
	if st := apply(op, 6, &wmeme, big.NewInt(0), contracts.ApproveCalldata(router, tokenAmt)); st != types.ReceiptSuccess {
		t.Fatal("approve wMeme failed")
	}
	if st := apply(op, 7, &wlxs, big.NewInt(0), contracts.ApproveCalldata(router, lxsAmt)); st != types.ReceiptSuccess {
		t.Fatal("approve wLXS failed")
	}
	deadline := new(big.Int).Lsh(big.NewInt(1), 62)
	addLiq := contracts.UniV2AddLiquidityCalldata(wmeme, wlxs, tokenAmt, lxsAmt, big.NewInt(0), big.NewInt(0), op.Address(), deadline)
	if st := apply(op, 8, &router, big.NewInt(0), addLiq); st != types.ReceiptSuccess {
		t.Fatal("addLiquidity failed")
	}

	// the pool now holds BOTH legs, and the operator's wrapped balances are drained into it.
	if got := readU(wmeme, contracts.BalanceOfCalldata(router)); got.Cmp(tokenAmt) != 0 {
		t.Fatalf("pool wMeme = %s, want %s", got, tokenAmt)
	}
	if got := readU(wlxs, contracts.BalanceOfCalldata(router)); got.Cmp(lxsAmt) != 0 {
		t.Fatalf("pool wLXS = %s, want %s", got, lxsAmt)
	}
	if got := readU(wmeme, contracts.BalanceOfCalldata(op.Address())); got.Sign() != 0 {
		t.Fatalf("operator wMeme left = %s, want 0 (all in the pool)", got)
	}
	if got := readU(wlxs, contracts.BalanceOfCalldata(op.Address())); got.Sign() != 0 {
		t.Fatalf("operator wLXS left = %s, want 0 (all in the pool)", got)
	}
}

// The graduation pool is seeded with the SAME wLXS the peg mints, so a graduation wLXS
// mint and a peg lock mint must never collide on wLXS's one shared mintedNonce mapping.
// GradWlxsMintNonce shifts graduation into a disjoint high range; this proves a peg mint
// at nonce 0 and a graduation mint at GradWlxsMintNonce(0) both land on the same token.
func TestGraduationWlxsNonceDisjointFromPeg(t *testing.T) {
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

	A, B := common.LXS(3), common.LXS(7)
	// a peg lock mint at nonce 0
	if st := apply(1, &wlxs, contracts.WlxsMintCalldata(big.NewInt(0), op.Address(), A)); st != types.ReceiptSuccess {
		t.Fatal("peg mint failed")
	}
	// a graduation mint at the shifted nonce for grad-nonce 0 — must NOT be seen as a replay
	if st := apply(2, &wlxs, contracts.WlxsMintCalldata(contracts.GradWlxsMintNonce(big.NewInt(0)), op.Address(), B)); st != types.ReceiptSuccess {
		t.Fatal("graduation mint collided with the peg's nonce 0 — nonce spaces are NOT disjoint")
	}
	out, err := Call(s, op.Address(), wlxs, contracts.TotalSupplyCalldata(), 3_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := new(big.Int).SetBytes(out), new(big.Int).Add(A, B); got.Cmp(want) != 0 {
		t.Fatalf("wLXS supply = %s, want %s (both mints landed)", got, want)
	}
}
