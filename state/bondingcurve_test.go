package state

import (
	"bytes"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/contracts"
	"lxs/crypto"
	"lxs/types"
)

// Bonding curve end to end: deploy the factory, create a coin (instantly
// tradeable, no upfront liquidity), buy, then sell it all back. Asserts the
// constant-product math against an independent reference, the 1% platform fee,
// and solvency: a full sell returns the buyer's native minus fees and leaves the
// curve at its start, so the virtual reserve is never paid out.
func TestBondingCurveCreateBuySellSolvent(t *testing.T) {
	deployer := key(t)
	buyer := key(t)
	feeRecipient := common.Address{0xFE}

	s := New()
	s.Credit(deployer.Address(), common.LXS(100))
	s.Credit(buyer.Address(), common.LXS(100))

	const feeBps = 100 // 1%
	virtualNative := new(big.Int).Mul(big.NewInt(30), big.NewInt(1e18))
	curveSupply := new(big.Int).Mul(big.NewInt(800_000_000), big.NewInt(1e18))

	apply := func(k *crypto.PrivateKey, nonce uint64, to *common.Address, value *big.Int, data []byte) (uint64, []*common.Log) {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 6_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		_, st, logs, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st, logs
	}
	callU := func(to common.Address, data []byte) *big.Int {
		out, err := Call(s, buyer.Address(), to, data, 5_000_000)
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		return new(big.Int).SetBytes(out)
	}

	// reference math for a buy.
	buyOut := func(nativeIn, reserveNative, curveTokens *big.Int) (out, inAmt, fee *big.Int) {
		fee = new(big.Int).Div(new(big.Int).Mul(nativeIn, big.NewInt(feeBps)), big.NewInt(10000))
		inAmt = new(big.Int).Sub(nativeIn, fee)
		eff := new(big.Int).Add(virtualNative, reserveNative)
		num := new(big.Int).Mul(eff, curveTokens)
		den := new(big.Int).Add(eff, inAmt)
		out = new(big.Int).Sub(curveTokens, new(big.Int).Div(num, den))
		return
	}

	// --- deploy factory ---
	if st, _ := apply(deployer, 0, nil, big.NewInt(0), contracts.PumpFactoryInit(feeRecipient, feeBps)); st != types.ReceiptSuccess {
		t.Fatal("factory deploy failed")
	}
	factory := CreateAddress(deployer.Address(), 0)

	// --- create a coin (one tx, no liquidity) ---
	st, logs := apply(buyer, 0, &factory, big.NewInt(0), contracts.PumpCreateCalldata("DogeLXS", "DOGE", nil, big.NewInt(0)))
	if st != types.ReceiptSuccess {
		t.Fatal("create failed")
	}
	var coin common.Address
	found := false
	for _, lg := range logs {
		if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpCreatedTopic() {
			coin = common.Address(lg.Data[12:32]) // first data word = coin address
			found = true
		}
	}
	if !found {
		t.Fatal("no Created event")
	}

	// --- buy with 1 LXS ---
	V := common.LXS(1)
	wantOut, inAmt, buyFee := buyOut(V, big.NewInt(0), curveSupply)
	if st, _ := apply(buyer, 1, &coin, V, contracts.PumpBuyCalldata(big.NewInt(0))); st != types.ReceiptSuccess {
		t.Fatal("buy failed")
	}
	if got := callU(coin, contracts.BalanceOfCalldata(buyer.Address())); got.Cmp(wantOut) != 0 {
		t.Fatalf("buyer tokens = %s, want reference %s", got, wantOut)
	}
	if got := callU(coin, contracts.PumpReserveNativeCalldata()); got.Cmp(inAmt) != 0 {
		t.Fatalf("reserveNative = %s, want in-after-fee %s", got, inAmt)
	}
	// The fee is ACCRUED in the contract (pull), not pushed to feeRecipient.
	if got := callU(coin, contracts.PumpFeeAccruedCalldata()); got.Cmp(buyFee) != 0 {
		t.Fatalf("accrued fee = %s, want 1%% of the buy = %s", got, buyFee)
	}
	if got := s.Balance(feeRecipient); got.Sign() != 0 {
		t.Fatalf("feeRecipient balance = %s, want 0 (fees are pulled, not pushed)", got)
	}
	// the contract holds reserve + the accrued fee.
	if got, want := s.Balance(coin), new(big.Int).Add(inAmt, buyFee); got.Cmp(want) != 0 {
		t.Fatalf("curve native balance = %s, want reserve+fee %s", got, want)
	}

	// --- sell all tokens back: the solvency proof ---
	tokens := new(big.Int).Set(wantOut)
	buyerNativeBefore := s.Balance(buyer.Address())
	if st, _ := apply(buyer, 2, &coin, big.NewInt(0), contracts.PumpSellCalldata(tokens, big.NewInt(0))); st != types.ReceiptSuccess {
		t.Fatal("sell failed — the curve could not honour a full sell (insolvent)")
	}
	// buyer holds no tokens; the curve is back to empty reserve (start state).
	if got := callU(coin, contracts.BalanceOfCalldata(buyer.Address())); got.Sign() != 0 {
		t.Fatalf("buyer tokens after selling all = %s, want 0", got)
	}
	if got := callU(coin, contracts.PumpReserveNativeCalldata()); got.Sign() != 0 {
		t.Fatalf("reserveNative after full sell = %s, want 0 (curve returned to start)", got)
	}
	// reserve is 0; only the accrued fees (buy + sell) remain in the contract.
	accrued := callU(coin, contracts.PumpFeeAccruedCalldata())
	if got := s.Balance(coin); got.Cmp(accrued) != 0 {
		t.Fatalf("curve native balance after full sell = %s, want the accrued fees %s", got, accrued)
	}
	if accrued.Cmp(buyFee) <= 0 {
		t.Fatalf("accrued fees = %s, want more than the buy fee %s (a sell fee was added)", accrued, buyFee)
	}
	// buyer got native back but less than it put in (paid both fees).
	gained := new(big.Int).Sub(s.Balance(buyer.Address()), buyerNativeBefore)
	if gained.Sign() <= 0 || gained.Cmp(inAmt) >= 0 {
		t.Fatalf("sell payout = %s, want (0, %s) — round trip must lose the fees", gained, inAmt)
	}

	// --- pull the fees out: only now does feeRecipient receive them, and trading was
	// never dependent on that transfer succeeding (a reverting recipient couldn't brick it). ---
	if st, _ := apply(buyer, 3, &coin, big.NewInt(0), contracts.PumpWithdrawFeesCalldata()); st != types.ReceiptSuccess {
		t.Fatal("withdrawFees failed")
	}
	if got := s.Balance(feeRecipient); got.Cmp(accrued) != 0 {
		t.Fatalf("feeRecipient after withdraw = %s, want the accrued fees %s", got, accrued)
	}
	if got := s.Balance(coin); got.Sign() != 0 {
		t.Fatalf("contract balance after withdrawFees = %s, want 0", got)
	}
}

// TestBondingCurveSlippageReverts: a buy demanding more tokens than the curve
// gives at this price reverts before any state change.
func TestBondingCurveSlippageReverts(t *testing.T) {
	deployer := key(t)
	buyer := key(t)

	s := New()
	s.Credit(deployer.Address(), common.LXS(100))
	s.Credit(buyer.Address(), common.LXS(100))

	apply := func(k *crypto.PrivateKey, nonce uint64, to *common.Address, value *big.Int, data []byte) (uint64, []*common.Log) {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 6_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		_, st, logs, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st, logs
	}

	apply(deployer, 0, nil, big.NewInt(0), contracts.PumpFactoryInit(common.Address{0xFE}, 100))
	factory := CreateAddress(deployer.Address(), 0)
	_, logs := apply(buyer, 0, &factory, big.NewInt(0), contracts.PumpCreateCalldata("X", "X", nil, big.NewInt(0)))
	var coin common.Address
	for _, lg := range logs {
		if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpCreatedTopic() {
			coin = common.Address(lg.Data[12:32])
		}
	}

	huge := new(big.Int).Mul(big.NewInt(1_000_000_000), big.NewInt(1e18))
	if st, _ := apply(buyer, 1, &coin, common.LXS(1), contracts.PumpBuyCalldata(huge)); st != types.ReceiptFailed {
		t.Fatal("a buy with an unreachable minTokensOut must revert")
	}
	if got := new(big.Int).SetBytes(func() []byte {
		o, _ := Call(s, buyer.Address(), coin, contracts.PumpReserveNativeCalldata(), 5_000_000)
		return o
	}()); got.Sign() != 0 {
		t.Fatalf("reserveNative moved on a reverted buy: %s", got)
	}
}

// A coin's thumbnail rides in the Created event log, not in storage — the site reads it
// straight from eth_getLogs. This verifies the exact bytes survive the round-trip, and that
// the on-chain size cap rejects an oversized blob so a coin cannot bloat every node's logs.
func TestPumpCreateCarriesImageInEvent(t *testing.T) {
	deployer := key(t)
	buyer := key(t)
	s := New()
	s.Credit(deployer.Address(), common.LXS(100))
	s.Credit(buyer.Address(), common.LXS(100))

	apply := func(k *crypto.PrivateKey, nonce uint64, to *common.Address, data []byte) (uint64, []*common.Log) {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: big.NewInt(0), GasLimit: 8_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		_, st, logs, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st, logs
	}

	if st, _ := apply(deployer, 0, nil, contracts.PumpFactoryInit(common.Address{0xFE}, 100)); st != types.ReceiptSuccess {
		t.Fatal("factory deploy failed")
	}
	factory := CreateAddress(deployer.Address(), 0)

	img := make([]byte, 777)
	for i := range img {
		img[i] = byte(i*7 + 1)
	}
	st, logs := apply(buyer, 0, &factory, contracts.PumpCreateCalldata("Pic", "PIC", img, big.NewInt(0)))
	if st != types.ReceiptSuccess {
		t.Fatal("create with image failed")
	}
	var data []byte
	for _, lg := range logs {
		if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpCreatedTopic() {
			data = lg.Data
		}
	}
	if data == nil {
		t.Fatal("no Created event emitted")
	}
	// event data: word0=coin, word1=off(name), word2=off(symbol), word3=off(image), then tails.
	off := new(big.Int).SetBytes(data[96:128]).Int64()
	n := new(big.Int).SetBytes(data[off : off+32]).Int64()
	got := data[off+32 : off+32+n]
	if !bytes.Equal(got, img) {
		t.Fatalf("image did not round-trip through the event: got %d bytes, want %d", len(got), len(img))
	}

	// over the cap must revert (a coin cannot bloat every node's logs); exactly at it is fine.
	if st, _ := apply(buyer, 1, &factory, contracts.PumpCreateCalldata("Big", "BIG", make([]byte, 12_289), big.NewInt(0))); st != types.ReceiptFailed {
		t.Fatal("SECURITY: an over-cap image was accepted")
	}
	if st, _ := apply(buyer, 2, &factory, contracts.PumpCreateCalldata("Ok", "OK", make([]byte, 12_288), big.NewInt(0))); st != types.ReceiptSuccess {
		t.Fatal("an image exactly at the cap was rejected")
	}
}

// Fix #2 (anti-snipe): create() with native value performs the creator's first buy in the
// SAME tx, crediting the creator — no separate opening-buy tx a sniper could front-run.
// With zero value, create() just deploys the coin (no buy).
func TestPumpCreateWithInitialBuyGoesToCreator(t *testing.T) {
	deployer := key(t)
	creator := key(t)
	feeRecipient := common.Address{0xFE}
	s := New()
	s.Credit(deployer.Address(), common.LXS(100))
	s.Credit(creator.Address(), common.LXS(100))

	apply := func(k *crypto.PrivateKey, nonce uint64, to *common.Address, value *big.Int, data []byte) (uint64, []*common.Log) {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 8_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		_, st, logs, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st, logs
	}
	callU := func(to common.Address, data []byte) *big.Int {
		out, err := Call(s, creator.Address(), to, data, 5_000_000)
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		return new(big.Int).SetBytes(out)
	}
	coinOf := func(logs []*common.Log) common.Address {
		for _, lg := range logs {
			if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpCreatedTopic() {
				return common.Address(lg.Data[12:32])
			}
		}
		t.Fatal("no Created event")
		return common.Address{}
	}

	apply(deployer, 0, nil, big.NewInt(0), contracts.PumpFactoryInit(feeRecipient, 100))
	factory := CreateAddress(deployer.Address(), 0)

	// create WITHOUT value: no initial buy, creator holds nothing.
	_, logs0 := apply(creator, 0, &factory, big.NewInt(0), contracts.PumpCreateCalldata("Plain", "PLN", nil, big.NewInt(0)))
	plain := coinOf(logs0)
	if got := callU(plain, contracts.BalanceOfCalldata(creator.Address())); got.Sign() != 0 {
		t.Fatalf("create with no value gave the creator %s tokens, want 0", got)
	}

	// create WITH 2 LXS: the creator must already hold curve tokens from the atomic buy.
	V := common.LXS(2)
	st, logs1 := apply(creator, 1, &factory, V, contracts.PumpCreateCalldata("Snipe", "SNP", nil, big.NewInt(0)))
	if st != types.ReceiptSuccess {
		t.Fatal("create+buy failed")
	}
	coin := coinOf(logs1)
	fee := new(big.Int).Div(new(big.Int).Mul(V, big.NewInt(100)), big.NewInt(10000))
	inAmt := new(big.Int).Sub(V, fee)

	if got := callU(coin, contracts.BalanceOfCalldata(creator.Address())); got.Sign() == 0 {
		t.Fatal("creator got NO tokens from the atomic first buy — the sniper window is open")
	}
	if got := callU(coin, contracts.PumpReserveNativeCalldata()); got.Cmp(inAmt) != 0 {
		t.Fatalf("reserveNative = %s, want in-after-fee %s", got, inAmt)
	}
	if got := callU(coin, contracts.PumpFeeAccruedCalldata()); got.Cmp(fee) != 0 {
		t.Fatalf("accrued fee = %s, want %s", got, fee)
	}
}

// Fix #1 (pull over push): a feeRecipient that reverts on receiving native must NOT be able
// to brick trading. buy/sell accrue the fee and succeed; only withdrawFees() (the isolated
// pull) fails against such a recipient. Uses a plain ERC-20 (no payable fallback) as the
// reverting recipient.
func TestPumpFeeReverterDoesNotBrickTrading(t *testing.T) {
	deployer := key(t)
	buyer := key(t)
	s := New()
	s.Credit(deployer.Address(), common.LXS(100))
	s.Credit(buyer.Address(), common.LXS(100))

	apply := func(k *crypto.PrivateKey, nonce uint64, to *common.Address, value *big.Int, data []byte) (uint64, []*common.Log) {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 8_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		_, st, logs, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st, logs
	}

	// a contract with no payable fallback: any plain value send to it reverts.
	apply(deployer, 0, nil, big.NewInt(0), contracts.UserTokenDeploy("Rev", "REV", common.LXS(1)))
	reverter := CreateAddress(deployer.Address(), 0)

	apply(deployer, 1, nil, big.NewInt(0), contracts.PumpFactoryInit(reverter, 100))
	factory := CreateAddress(deployer.Address(), 1)

	_, logs := apply(buyer, 0, &factory, big.NewInt(0), contracts.PumpCreateCalldata("Doge", "DOGE", nil, big.NewInt(0)))
	var coin common.Address
	for _, lg := range logs {
		if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpCreatedTopic() {
			coin = common.Address(lg.Data[12:32])
		}
	}

	// buy MUST succeed even though the fee recipient would revert on receipt.
	if st, _ := apply(buyer, 1, &coin, common.LXS(2), contracts.PumpBuyCalldata(big.NewInt(0))); st != types.ReceiptSuccess {
		t.Fatal("SECURITY: a reverting feeRecipient bricked buy() — push payment, not pull")
	}
	// sell too.
	half := new(big.Int).Div(new(big.Int).SetBytes(func() []byte {
		o, _ := Call(s, buyer.Address(), coin, contracts.BalanceOfCalldata(buyer.Address()), 5_000_000)
		return o
	}()), big.NewInt(2))
	if st, _ := apply(buyer, 2, &coin, big.NewInt(0), contracts.PumpSellCalldata(half, big.NewInt(0))); st != types.ReceiptSuccess {
		t.Fatal("SECURITY: a reverting feeRecipient bricked sell()")
	}
	// withdrawFees() is the ONLY thing that fails against the bad recipient — trading was fine.
	if st, _ := apply(buyer, 3, &coin, big.NewInt(0), contracts.PumpWithdrawFeesCalldata()); st != types.ReceiptFailed {
		t.Fatal("withdrawFees to a reverting recipient should fail (but must not affect trading)")
	}
}
