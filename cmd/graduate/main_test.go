package main

import (
	"encoding/hex"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/contracts"
)

// parseGraduated must decode the exact layout the GraduationVault emits:
// Graduated(uint256 indexed nonce, address indexed coin, address indexed from,
// uint256 lxsAmount, uint256 tokenAmount) — three indexed topics after the signature,
// then two data words. A layout slip here silently pools the wrong coin/amounts.
func TestParseGraduatedMatchesEventLayout(t *testing.T) {
	coin, _ := common.AddressFromHex("0x00000000000000000000000000000000c0ffee01")
	from, _ := common.AddressFromHex("0x00000000000000000000000000000000decafbad")
	nonce := big.NewInt(42)
	lxsAmount := common.LXS(5)
	tokenAmount := common.LXS(1000)

	l := ethLog{
		Topics: []string{
			contracts.GraduatedTopic().Hex(),
			"0x" + hex.EncodeToString(word(nonce)),
			"0x" + hex.EncodeToString(addr32(coin)),
			"0x" + hex.EncodeToString(addr32(from)),
		},
		Data:        "0x" + hex.EncodeToString(word(lxsAmount)) + hex.EncodeToString(word(tokenAmount)),
		BlockNumber: "0x10",
	}

	g, ok := parseGraduated(l)
	if !ok {
		t.Fatal("parseGraduated rejected a well-formed event")
	}
	if g.nonce.Cmp(nonce) != 0 {
		t.Fatalf("nonce = %s, want %s", g.nonce, nonce)
	}
	if g.coin != coin {
		t.Fatalf("coin = %s, want %s", g.coin.Hex(), coin.Hex())
	}
	if g.lxsAmount.Cmp(lxsAmount) != 0 {
		t.Fatalf("lxsAmount = %s, want %s", g.lxsAmount, lxsAmount)
	}
	if g.tokenAmount.Cmp(tokenAmount) != 0 {
		t.Fatalf("tokenAmount = %s, want %s", g.tokenAmount, tokenAmount)
	}
}

// Malformed logs must be rejected, not mis-parsed into a bogus graduation.
func TestParseGraduatedRejectsMalformed(t *testing.T) {
	full := ethLog{
		Topics: []string{contracts.GraduatedTopic().Hex(), "0x01", "0x02", "0x03"},
		Data:   "0x" + hex.EncodeToString(word(big.NewInt(1))) + hex.EncodeToString(word(big.NewInt(2))),
	}
	// too few topics (missing the indexed `from`)
	short := full
	short.Topics = full.Topics[:3]
	if _, ok := parseGraduated(short); ok {
		t.Fatal("accepted an event with too few topics")
	}
	// truncated data (only one word instead of two)
	trunc := full
	trunc.Data = "0x" + hex.EncodeToString(word(big.NewInt(1)))
	if _, ok := parseGraduated(trunc); ok {
		t.Fatal("accepted an event with truncated data")
	}
}
