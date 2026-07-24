package contracts

import (
	"testing"

	"lxs/common"
)

func TestBondingCurveABIConstants(t *testing.T) {
	sel := func(sig string) uint32 {
		b := common.Keccak256([]byte(sig)).Bytes()
		return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	}
	cases := []struct {
		sig  string
		want uint32
	}{
		{"create(string,string,bytes,uint256)", PumpCreateSelector},
		{"buy(uint256)", PumpBuySelector},
		{"sell(uint256,uint256)", PumpSellSelector},
		{"quoteBuy(uint256)", PumpQuoteBuy},
		{"reserveNative()", PumpReserveNative},
		{"curveTokens()", PumpCurveTokens},
	}
	for _, c := range cases {
		if got := sel(c.sig); got != c.want {
			t.Fatalf("selector for %q = %#x, want %#x", c.sig, c.want, got)
		}
	}
	if PumpCreatedTopic() != common.Keccak256([]byte("Created(address,address,string,string,bytes)")) {
		t.Fatal("PumpCreatedTopic drifted")
	}
	if len(PumpFactoryInit(common.Address{0x1}, 100, common.Address{}, common.Address{})) < 1000 {
		t.Fatal("PumpFactory bytecode did not embed")
	}
}
