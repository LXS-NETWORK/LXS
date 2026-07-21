package types

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"lxs/common"
)

// TestParseCanonicalEIP155Vector checks MetaMask compatibility against the
// worked example from the EIP-155 spec. Parsing these exact bytes and recovering
// this sender means any transaction MetaMask signs for this chain verifies here.
//
//	nonce 9, gasPrice 20 Gwei, gas 21000, to 0x3535…3535, value 1 ETH,
//	chainId 1  ->  sender 0x9d8A62f656a8d1615C1294fd71e9CFb3E4855A4F
func TestParseCanonicalEIP155Vector(t *testing.T) {
	raw, err := hex.DecodeString("f86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := ParseEthLegacyTx(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Fields decoded from the RLP.
	if tx.Nonce != 9 {
		t.Errorf("nonce = %d, want 9", tx.Nonce)
	}
	if tx.ChainID != 1 {
		t.Errorf("chainId = %d, want 1", tx.ChainID)
	}
	if tx.GasLimit != 21000 {
		t.Errorf("gasLimit = %d, want 21000", tx.GasLimit)
	}
	if tx.GasPrice.String() != "20000000000" {
		t.Errorf("gasPrice = %s, want 20000000000", tx.GasPrice)
	}
	if tx.Value.String() != "1000000000000000000" {
		t.Errorf("value = %s, want 1e18", tx.Value)
	}
	if tx.To == nil || tx.To.Hex() != "0x3535353535353535353535353535353535353535" {
		t.Errorf("to = %v, want 0x3535…3535", tx.To)
	}

	// The recovered signer must be the spec's sender.
	sender, err := tx.Sender()
	if err != nil {
		t.Fatalf("recover sender: %v", err)
	}
	want, _ := common.AddressFromHex("0x9d8a62f656a8d1615c1294fd71e9cfb3e4855a4f")
	if sender != want {
		t.Fatalf("recovered sender = %s, want the EIP-155 spec sender %s", sender.Hex(), want.Hex())
	}

	// The tx identity is keccak256 of the raw bytes, what a wallet tracks.
	if tx.Hash() != common.Keccak256(raw) {
		t.Fatalf("tx hash = %s, want keccak256(raw) = %s", tx.Hash().Hex(), common.Keccak256(raw).Hex())
	}
}

// TestEthTxRejectsTamper checks the signature is binding: doubling the value
// changes the recovered sender (or fails recovery).
func TestEthTxRejectsTamper(t *testing.T) {
	raw, _ := hex.DecodeString("f86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83")
	tx, _ := ParseEthLegacyTx(raw)
	orig, _ := tx.Sender()

	// Rebuild with the value doubled but the same signature.
	tampered := &Transaction{
		Type: TxTypeEthLegacy, ChainID: tx.ChainID, Nonce: tx.Nonce, To: tx.To,
		Value: tx.Value.Add(tx.Value, tx.Value), GasLimit: tx.GasLimit, GasPrice: tx.GasPrice,
		Data: tx.Data, Sig: tx.Sig,
	}
	got, err := tampered.Sender()
	if err == nil && got == orig {
		t.Fatal("tampering with the value kept the original signer — the signature is not binding")
	}
}

// TestEthTxSurvivesJSONRoundTrip is a consensus-safety test. Eth-legacy txs are
// gossiped and stored as JSON; losing the type marker would make a receiving
// node recompute the signing hash the native way, recover a different sender,
// and split the chain. This checks the type, sender and tx hash survive the trip.
func TestEthTxSurvivesJSONRoundTrip(t *testing.T) {
	raw, _ := hex.DecodeString("f86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83")
	tx, err := ParseEthLegacyTx(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantSender, _ := tx.Sender()
	wantHash := tx.Hash()

	data, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	var tx2 Transaction
	if err := json.Unmarshal(data, &tx2); err != nil {
		t.Fatal(err)
	}
	if tx2.Type != TxTypeEthLegacy {
		t.Fatal("the eth-legacy type marker was lost across JSON — nodes would split")
	}
	gotSender, err := tx2.Sender()
	if err != nil {
		t.Fatalf("sender recovery failed after round-trip: %v", err)
	}
	if gotSender != wantSender {
		t.Fatalf("sender changed across serialization: %s != %s", gotSender.Hex(), wantSender.Hex())
	}
	if tx2.Hash() != wantHash {
		t.Fatalf("tx hash changed across serialization: %s != %s", tx2.Hash().Hex(), wantHash.Hex())
	}
}
