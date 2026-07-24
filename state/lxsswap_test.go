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

// The LXS-native Uniswap-V2 AMM end to end on the VM: deploy WLXS + factory + a test
// ERC-20, create the pair, add liquidity, swap, and assert the constant-product
// invariant. This is the "DEX-market" a price aggregator reads — so the test also
// pins that the canonical V2 views return sane reserves after each step.
func TestLxsSwapAddLiquidityAndSwap(t *testing.T) {
	op := key(t)
	s := New()
	s.Credit(op.Address(), common.LXS(1_000_000))

	apply := func(nonce uint64, to *common.Address, value *big.Int, data []byte) uint64 {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 6_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(op); err != nil {
			t.Fatal(err)
		}
		_, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st
	}
	call := func(to common.Address, data []byte) []byte {
		out, err := Call(s, op.Address(), to, data, 6_000_000)
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		return out
	}
	readU := func(to common.Address, data []byte) *big.Int { return new(big.Int).SetBytes(call(to, data)) }

	// deploy WLXS (0), factory (1), test token (2)
	if st := apply(0, nil, big.NewInt(0), contracts.WlxsInit()); st != types.ReceiptSuccess {
		t.Fatal("WLXS deploy failed")
	}
	wlxs := CreateAddress(op.Address(), 0)
	if st := apply(1, nil, big.NewInt(0), contracts.LxsSwapFactoryInit(op.Address())); st != types.ReceiptSuccess {
		t.Fatal("factory deploy failed")
	}
	factory := CreateAddress(op.Address(), 1)
	supply := new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(1e18))
	if st := apply(2, nil, big.NewInt(0), contracts.UserTokenDeploy("Test", "TST", supply)); st != types.ReceiptSuccess {
		t.Fatal("token deploy failed")
	}
	token := CreateAddress(op.Address(), 2)

	// create the pair
	if st := apply(3, &factory, big.NewInt(0), contracts.SwapCreatePairCalldata(token, wlxs)); st != types.ReceiptSuccess {
		t.Fatal("createPair failed")
	}
	pairWord := call(factory, contracts.SwapGetPairCalldata(token, wlxs))
	if len(pairWord) < 32 {
		t.Fatalf("getPair short output: %d bytes", len(pairWord))
	}
	pair := common.Address(pairWord[12:32]) // address right-aligned in the word
	if (pair == common.Address{}) {
		t.Fatal("pair not registered")
	}

	// wrap 110 LXS -> WLXS (100 seeds the pool, the rest funds the test swaps)
	if st := apply(4, &wlxs, common.LXS(110), contracts.WlxsDepositCalldata()); st != types.ReceiptSuccess {
		t.Fatal("wrap failed")
	}

	// add liquidity: send 100000 TST + 100 WLXS to the pair, then mint LP
	tokAmt := new(big.Int).Mul(big.NewInt(100_000), big.NewInt(1e18))
	lxsAmt := common.LXS(100)
	if st := apply(5, &token, big.NewInt(0), contracts.TransferCalldata(pair, tokAmt)); st != types.ReceiptSuccess {
		t.Fatal("token->pair transfer failed")
	}
	if st := apply(6, &wlxs, big.NewInt(0), contracts.TransferCalldata(pair, lxsAmt)); st != types.ReceiptSuccess {
		t.Fatal("wlxs->pair transfer failed")
	}
	if st := apply(7, &pair, big.NewInt(0), contracts.SwapPairMintCalldata(op.Address())); st != types.ReceiptSuccess {
		t.Fatal("mint LP failed")
	}
	if lp := readU(pair, contracts.BalanceOfCalldata(op.Address())); lp.Sign() <= 0 {
		t.Fatalf("no LP minted, got %s", lp)
	}

	// reserves reflect the deposit (order = sorted addresses)
	r0, r1 := reserves(t, call(pair, contracts.SwapPairGetReservesCalldata()))
	tokenIs0 := bytes.Compare(token[:], wlxs[:]) < 0
	rTok, rLxs := r1, r0
	if tokenIs0 {
		rTok, rLxs = r0, r1
	}
	if rTok.Cmp(tokAmt) != 0 || rLxs.Cmp(lxsAmt) != 0 {
		t.Fatalf("reserves wrong: token %s (want %s), wlxs %s (want %s)", rTok, tokAmt, rLxs, lxsAmt)
	}

	// swap 1 WLXS -> TST. transfer input in, then call swap for the computed output.
	in := common.LXS(1)
	out := getAmountOut(in, rLxs, rTok) // 0.30% fee
	if st := apply(8, &wlxs, big.NewInt(0), contracts.TransferCalldata(pair, in)); st != types.ReceiptSuccess {
		t.Fatal("swap input transfer failed")
	}
	var swapData []byte
	if tokenIs0 { // token is token0 -> it is the amount0Out side
		swapData = contracts.SwapPairSwapCalldata(out, big.NewInt(0), op.Address())
	} else {
		swapData = contracts.SwapPairSwapCalldata(big.NewInt(0), out, op.Address())
	}
	before := readU(token, contracts.BalanceOfCalldata(op.Address()))
	if st := apply(9, &pair, big.NewInt(0), swapData); st != types.ReceiptSuccess {
		t.Fatal("swap failed (K invariant should have held for the computed output)")
	}
	after := readU(token, contracts.BalanceOfCalldata(op.Address()))
	if new(big.Int).Sub(after, before).Cmp(out) != 0 {
		t.Fatalf("swap payout wrong: got %s want %s", new(big.Int).Sub(after, before), out)
	}

	// SABOTAGE: asking for 1 wei MORE than the fee-preserving output must break K and revert.
	if st := apply(10, &wlxs, big.NewInt(0), contracts.TransferCalldata(pair, in)); st != types.ReceiptSuccess {
		t.Fatal("second input transfer failed")
	}
	r0b, r1b := reserves(t, call(pair, contracts.SwapPairGetReservesCalldata()))
	rLxsB, rTokB := r0b, r1b
	if tokenIs0 {
		rLxsB, rTokB = r1b, r0b
	}
	greedy := new(big.Int).Add(getAmountOut(in, rLxsB, rTokB), big.NewInt(1))
	var badData []byte
	if tokenIs0 {
		badData = contracts.SwapPairSwapCalldata(greedy, big.NewInt(0), op.Address())
	} else {
		badData = contracts.SwapPairSwapCalldata(big.NewInt(0), greedy, op.Address())
	}
	if st := apply(11, &pair, big.NewInt(0), badData); st != types.ReceiptFailed {
		t.Fatal("SABOTAGE FAILED: an over-payout swap (K violated) was accepted — the invariant is not enforced")
	}
}

// reserves parses getReserves() -> (uint112 reserve0, uint112 reserve1, uint32 ts).
func reserves(t *testing.T, out []byte) (*big.Int, *big.Int) {
	t.Helper()
	if len(out) < 64 {
		t.Fatalf("getReserves short output: %d bytes", len(out))
	}
	return new(big.Int).SetBytes(out[0:32]), new(big.Int).SetBytes(out[32:64])
}

// getAmountOut mirrors the pair's 0.30%-fee constant-product math.
func getAmountOut(amountIn, reserveIn, reserveOut *big.Int) *big.Int {
	inWithFee := new(big.Int).Mul(amountIn, big.NewInt(997))
	num := new(big.Int).Mul(inWithFee, reserveOut)
	den := new(big.Int).Add(new(big.Int).Mul(reserveIn, big.NewInt(1000)), inWithFee)
	return new(big.Int).Div(num, den)
}

var _ = crypto.PrivateKey{}

// The launchpad end to end: a coin created by a graduation-wired PumpFactory auto-seeds
// a real COIN/WLXS pool the moment its curve takes GRAD_TARGET (300) native LXS — no
// button, funded by the curve's own reserves — then the curve closes and the token lives
// on the pool a price aggregator can index.
func TestPumpCoinAutoGraduatesToPool(t *testing.T) {
	op := key(t)
	s := New()
	s.Credit(op.Address(), common.LXS(2000))

	apply := func(nonce uint64, to *common.Address, value *big.Int, data []byte) (uint64, []*common.Log) {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 8_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(op); err != nil {
			t.Fatal(err)
		}
		_, st, logs, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st, logs
	}
	readU := func(to common.Address, data []byte) *big.Int {
		out, err := Call(s, op.Address(), to, data, 6_000_000)
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		return new(big.Int).SetBytes(out)
	}

	// DEX + launchpad, graduation wired
	apply(0, nil, big.NewInt(0), contracts.WlxsInit())
	wlxs := CreateAddress(op.Address(), 0)
	apply(1, nil, big.NewInt(0), contracts.LxsSwapFactoryInit(op.Address()))
	swapFactory := CreateAddress(op.Address(), 1)
	if st, _ := apply(2, nil, big.NewInt(0), contracts.PumpFactoryInit(op.Address(), 100, swapFactory, wlxs)); st != types.ReceiptSuccess {
		t.Fatal("PumpFactory deploy failed")
	}
	pumpFactory := CreateAddress(op.Address(), 2)

	// create a coin
	_, logs := apply(3, &pumpFactory, big.NewInt(0), contracts.PumpCreateCalldata("Grad", "GRD", nil, big.NewInt(0)))
	var coin common.Address
	for _, lg := range logs {
		if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpCreatedTopic() {
			coin = common.Address(lg.Data[12:32])
		}
	}
	if (coin == common.Address{}) {
		t.Fatal("no Created event")
	}

	graduatedSel := []byte{0xe7, 0xc2, 0xb7, 0x72} // graduated()
	poolSel := []byte{0x16, 0xf0, 0x11, 0x5b}      // pool()
	if readU(coin, graduatedSel).Sign() != 0 {
		t.Fatal("coin graduated before any buy")
	}

	// one buy big enough to cross GRAD_TARGET (300): 320 LXS, ~316.8 after the 1% fee.
	st, blogs := apply(4, &coin, common.LXS(320), contracts.PumpBuyCalldata(big.NewInt(0)))
	if st != types.ReceiptSuccess {
		t.Fatal("graduating buy failed")
	}

	// it must have graduated
	if readU(coin, graduatedSel).Sign() == 0 {
		t.Fatal("curve took 316 LXS (> 300 target) but did not graduate")
	}
	poolWord, _ := Call(s, op.Address(), coin, poolSel, 3_000_000)
	pool := common.Address(poolWord[12:32])
	if (pool == common.Address{}) {
		t.Fatal("graduated but pool address is zero")
	}
	// the Graduated event announces the pool
	sawGrad := false
	for _, lg := range blogs {
		if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpGraduatedTopic() {
			sawGrad = true
		}
	}
	if !sawGrad {
		t.Fatal("no Graduated event emitted")
	}

	// the pool is a real, seeded LxsSwap pair: both reserves > 0 and equal to the balances
	r0, r1 := reserves(t, mustCall(t, s, op.Address(), pool, contracts.SwapPairGetReservesCalldata()))
	if r0.Sign() <= 0 || r1.Sign() <= 0 {
		t.Fatalf("pool not seeded: reserves %s / %s", r0, r1)
	}
	tokenIs0 := bytes.Compare(coin[:], wlxs[:]) < 0
	rTok, rLxs := r1, r0
	if tokenIs0 {
		rTok, rLxs = r0, r1
	}
	if got := readU(coin, contracts.BalanceOfCalldata(pool)); got.Cmp(rTok) != 0 {
		t.Fatalf("coin balance in pool %s != reserve %s", got, rTok)
	}
	if got := readU(wlxs, contracts.BalanceOfCalldata(pool)); got.Cmp(rLxs) != 0 {
		t.Fatalf("wlxs balance in pool %s != reserve %s", got, rLxs)
	}
	// LP is locked forever at address(0) — nobody can pull the liquidity (no rug)
	if lp := readU(pool, contracts.BalanceOfCalldata(common.Address{})); lp.Sign() <= 0 {
		t.Fatal("LP not locked at address(0) — liquidity is rug-pullable")
	}

	// the curve is CLOSED: further buys and sells revert (trading moves to the pool)
	if st, _ := apply(5, &coin, common.LXS(1), contracts.PumpBuyCalldata(big.NewInt(0))); st != types.ReceiptFailed {
		t.Fatal("a buy after graduation was accepted — the curve should be closed")
	}
	if st, _ := apply(6, &coin, big.NewInt(0), contracts.PumpSellCalldata(big.NewInt(1000), big.NewInt(0))); st != types.ReceiptFailed {
		t.Fatal("a sell after graduation was accepted — the curve should be closed")
	}
}

func mustCall(t *testing.T, s *State, from, to common.Address, data []byte) []byte {
	t.Helper()
	out, err := Call(s, from, to, data, 6_000_000)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	return out
}

// After a coin graduates, the router must let a wallet BUY (native LXS -> token) and
// SELL (token -> native LXS) against the pool in one atomic tx each. Proves the whole
// trade path a UI uses, with the payout landing in native LXS.
func TestLxsSwapRouterTradesGraduatedPool(t *testing.T) {
	op := key(t)
	s := New()
	s.Credit(op.Address(), common.LXS(2000))

	apply := func(nonce uint64, to *common.Address, value *big.Int, data []byte) (uint64, []*common.Log) {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 8_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(op); err != nil {
			t.Fatal(err)
		}
		_, st, logs, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st, logs
	}
	readU := func(to common.Address, data []byte) *big.Int {
		return new(big.Int).SetBytes(mustCall(t, s, op.Address(), to, data))
	}

	apply(0, nil, big.NewInt(0), contracts.WlxsInit())
	wlxs := CreateAddress(op.Address(), 0)
	apply(1, nil, big.NewInt(0), contracts.LxsSwapFactoryInit(op.Address()))
	swapFactory := CreateAddress(op.Address(), 1)
	apply(2, nil, big.NewInt(0), contracts.PumpFactoryInit(op.Address(), 100, swapFactory, wlxs))
	pumpFactory := CreateAddress(op.Address(), 2)
	apply(3, nil, big.NewInt(0), contracts.LxsSwapRouterInit(swapFactory, wlxs))
	router := CreateAddress(op.Address(), 3)

	// create + graduate in one tx (320 LXS > 300 target)
	_, logs := apply(4, &pumpFactory, common.LXS(320), contracts.PumpCreateCalldata("Grad", "GRD", nil, big.NewInt(0)))
	var coin common.Address
	for _, lg := range logs {
		if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpCreatedTopic() {
			coin = common.Address(lg.Data[12:32])
		}
	}
	if readU(coin, []byte{0xe7, 0xc2, 0xb7, 0x72}).Sign() == 0 {
		t.Fatal("coin did not graduate")
	}

	huge := new(big.Int).Lsh(big.NewInt(1), 60) // deadline far in the future

	// BUY via router: 5 LXS -> coin, paid out to op
	coinBefore := readU(coin, contracts.BalanceOfCalldata(op.Address()))
	if st, _ := apply(5, &router, common.LXS(5), contracts.RouterBuyCalldata(coin, big.NewInt(0), op.Address(), huge)); st != types.ReceiptSuccess {
		t.Fatal("router BUY failed")
	}
	coinAfter := readU(coin, contracts.BalanceOfCalldata(op.Address()))
	bought := new(big.Int).Sub(coinAfter, coinBefore)
	if bought.Sign() <= 0 {
		t.Fatalf("router BUY paid out no tokens (before %s after %s)", coinBefore, coinAfter)
	}

	// SELL via router: approve, then sell the tokens just bought back for native LXS
	if st, _ := apply(6, &coin, big.NewInt(0), contracts.ApproveCalldata(router, bought)); st != types.ReceiptSuccess {
		t.Fatal("approve router failed")
	}
	lxsBefore := s.Balance(op.Address())
	if st, _ := apply(7, &router, big.NewInt(0), contracts.RouterSellCalldata(coin, bought, big.NewInt(0), op.Address(), huge)); st != types.ReceiptSuccess {
		t.Fatal("router SELL failed")
	}
	lxsAfter := s.Balance(op.Address())
	// op paid gas too, but the sell payout is ~4.98 LXS which dwarfs the tiny gas; net must be up
	if lxsAfter.Cmp(lxsBefore) <= 0 {
		t.Fatalf("router SELL returned no native LXS (before %s after %s)", lxsBefore, lxsAfter)
	}
}

// SABOTAGE / fix-verification: the graduation pool must be UN-pre-seedable. A griefer
// who seeds the COIN/WLXS pool before graduation with a skewed ratio could otherwise
// skim the graduation liquidity. The pool is created gated to the coin at construction,
// so every attacker path to a pre-mint must revert; graduation itself must still work.
func TestGraduationPoolIsUnseedable(t *testing.T) {
	op := key(t)
	att := key(t)
	s := New()
	s.Credit(op.Address(), common.LXS(2000))
	s.Credit(att.Address(), common.LXS(2000))

	send := func(signer *crypto.PrivateKey, nonce uint64, to *common.Address, value *big.Int, data []byte) (uint64, []*common.Log) {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: value, GasLimit: 8_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(signer); err != nil {
			t.Fatal(err)
		}
		_, st, logs, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st, logs
	}
	readU := func(to common.Address, data []byte) *big.Int {
		return new(big.Int).SetBytes(mustCall(t, s, op.Address(), to, data))
	}

	send(op, 0, nil, big.NewInt(0), contracts.WlxsInit())
	wlxs := CreateAddress(op.Address(), 0)
	send(op, 1, nil, big.NewInt(0), contracts.LxsSwapFactoryInit(op.Address()))
	swapFactory := CreateAddress(op.Address(), 1)
	send(op, 2, nil, big.NewInt(0), contracts.PumpFactoryInit(op.Address(), 100, swapFactory, wlxs))
	pumpFactory := CreateAddress(op.Address(), 2)

	_, logs := send(op, 3, &pumpFactory, big.NewInt(0), contracts.PumpCreateCalldata("Grad", "GRD", nil, big.NewInt(0)))
	var coin common.Address
	for _, lg := range logs {
		if len(lg.Topics) > 0 && lg.Topics[0] == contracts.PumpCreatedTopic() {
			coin = common.Address(lg.Data[12:32])
		}
	}
	// the coin owns its gated pool from birth
	pool := common.Address(mustCall(t, s, op.Address(), coin, []byte{0x16, 0xf0, 0x11, 0x5b})[12:32]) // pool()
	if (pool == common.Address{}) {
		t.Fatal("coin has no pool at construction")
	}
	grad := common.Address(mustCall(t, s, op.Address(), pool, []byte{0x02, 0x45, 0xa4, 0x83})[12:32]) // graduator()
	if grad != coin {
		t.Fatalf("pool graduator is %s, expected the coin %s", grad, coin)
	}

	// ATTACK 1: attacker buys coin on the curve, then tries to seed+mint the pool first.
	if st, _ := send(att, 0, &coin, common.LXS(50), contracts.PumpBuyCalldata(big.NewInt(0))); st != types.ReceiptSuccess {
		t.Fatal("attacker curve buy failed")
	}
	if st, _ := send(att, 1, &wlxs, common.LXS(50), contracts.WlxsDepositCalldata()); st != types.ReceiptSuccess {
		t.Fatal("attacker wrap failed")
	}
	attCoin := readU(coin, contracts.BalanceOfCalldata(att.Address()))
	send(att, 2, &coin, big.NewInt(0), contracts.TransferCalldata(pool, attCoin))
	send(att, 3, &wlxs, big.NewInt(0), contracts.TransferCalldata(pool, common.LXS(50)))
	if st, _ := send(att, 4, &pool, big.NewInt(0), contracts.SwapPairMintCalldata(att.Address())); st != types.ReceiptFailed {
		t.Fatal("ATTACK SUCCEEDED: attacker pre-seeded the graduation pool (mint gate broken)")
	}

	// ATTACK 2: attacker cannot gate a fresh pair to themselves...
	gatedToAtt := append(append(append([]byte{0x3e, 0x6d, 0x7c, 0x65}, leftPad(coin[:])...), leftPad(wlxs[:])...), leftPad(att.Address().Bytes())...)
	if st, _ := send(att, 5, &swapFactory, big.NewInt(0), gatedToAtt); st != types.ReceiptFailed {
		t.Fatal("ATTACK SUCCEEDED: attacker gated the coin's pair to themselves")
	}
	// ...and cannot even create a plain pair for the coin (it already exists, gated to the coin)
	if st, _ := send(att, 6, &swapFactory, big.NewInt(0), contracts.SwapCreatePairCalldata(coin, wlxs)); st != types.ReceiptFailed {
		t.Fatal("ATTACK SUCCEEDED: attacker created a rival pair for the coin")
	}

	// Graduation must STILL work and seed the coin's own gated pool.
	if st, _ := send(op, 4, &coin, common.LXS(320), contracts.PumpBuyCalldata(big.NewInt(0))); st != types.ReceiptSuccess {
		t.Fatal("legit graduating buy failed after the attacks")
	}
	if readU(coin, []byte{0xe7, 0xc2, 0xb7, 0x72}).Sign() == 0 {
		t.Fatal("coin failed to graduate after surviving the attacks")
	}
	// pool is seeded and the LP is locked at address(0)
	r0, r1 := reserves(t, mustCall(t, s, op.Address(), pool, contracts.SwapPairGetReservesCalldata()))
	if r0.Sign() <= 0 || r1.Sign() <= 0 {
		t.Fatalf("pool not seeded after graduation: %s / %s", r0, r1)
	}
	if lp := readU(pool, contracts.BalanceOfCalldata(common.Address{})); lp.Sign() <= 0 {
		t.Fatal("graduation LP not locked at address(0)")
	}
}

// leftPad left-pads bytes to a 32-byte word (address -> ABI word).
func leftPad(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}
