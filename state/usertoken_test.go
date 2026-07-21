package state

import (
	"math/big"
	"testing"

	"lxs/common"
	"lxs/contracts"
	"lxs/types"
)

// The create-token flow: deploy UserToken with a name, symbol, and supply, all
// minted to the deployer, then use it.
func TestUserTokenCreateAndUse(t *testing.T) {
	dev := key(t)
	bob := key(t).Address()

	s := New()
	s.Credit(dev.Address(), common.LXS(100))

	apply := func(nonce uint64, to *common.Address, data []byte) uint64 {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: big.NewInt(0), GasLimit: 3_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(dev); err != nil {
			t.Fatal(err)
		}
		_, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored: %v", err)
		}
		return st
	}
	callStr := func(tok common.Address, data []byte) string {
		out, err := Call(s, dev.Address(), tok, data, 3_000_000)
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		if len(out) < 64 {
			return ""
		}
		n := new(big.Int).SetBytes(out[32:64]).Int64()
		return string(out[64 : 64+n])
	}
	callU := func(tok common.Address, data []byte) *big.Int {
		out, err := Call(s, dev.Address(), tok, data, 3_000_000)
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		return new(big.Int).SetBytes(out)
	}

	supply := new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(1e18))
	if st := apply(0, nil, contracts.UserTokenDeploy("MemeCoin", "MEME", supply)); st != types.ReceiptSuccess {
		t.Fatal("token deploy failed")
	}
	tok := CreateAddress(dev.Address(), 0)

	if got := callStr(tok, contracts.NameCalldata()); got != "MemeCoin" {
		t.Fatalf("name() = %q, want MemeCoin", got)
	}
	if got := callStr(tok, contracts.SymbolCalldata()); got != "MEME" {
		t.Fatalf("symbol() = %q, want MEME", got)
	}
	if got := callU(tok, contracts.TotalSupplyCalldata()); got.Cmp(supply) != 0 {
		t.Fatalf("totalSupply = %s, want %s", got, supply)
	}
	if got := callU(tok, contracts.BalanceOfCalldata(dev.Address())); got.Cmp(supply) != 0 {
		t.Fatalf("creator balance = %s, want the whole supply %s", got, supply)
	}

	// the token is usable: transfer 250k to bob.
	amt := new(big.Int).Mul(big.NewInt(250_000), big.NewInt(1e18))
	if st := apply(1, &tok, contracts.TransferCalldata(bob, amt)); st != types.ReceiptSuccess {
		t.Fatal("transfer failed")
	}
	if got := callU(tok, contracts.BalanceOfCalldata(bob)); got.Cmp(amt) != 0 {
		t.Fatalf("bob balance = %s, want %s", got, amt)
	}
}
