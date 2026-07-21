package state

import (
	"bytes"
	"encoding/binary"
	"math/big"
	"testing"

	"lxs/common"
)

// TestAccountEncodeInjective: the account commitment (and thus the state root)
// must be injective. Constructs the collision the old unprefixed encoding allowed
// — a storage-only and a code-only account whose bytes coincide — and asserts they
// encode differently.
//
// Code is 56 bytes, so Bytes(code) writes an 8-byte length (=56) then 56 bytes =
// 64 bytes, matching one 32-byte key ‖ 32-byte value slot. The storage count
// prefix now separates them.
func TestAccountEncodeInjective(t *testing.T) {
	code := make([]byte, 56)
	for i := range code {
		code[i] = byte(i + 1)
	}
	var key, val common.Hash
	binary.BigEndian.PutUint64(key[0:8], 56) // the 8-byte length prefix the code would carry
	copy(key[8:32], code[0:24])
	copy(val[0:32], code[24:56])

	accStorage := &Account{Nonce: 3, Balance: big.NewInt(7), Storage: map[common.Hash]common.Hash{key: val}}
	accCode := &Account{Nonce: 3, Balance: big.NewInt(7), Code: code}

	if bytes.Equal(accStorage.encode(), accCode.encode()) {
		t.Fatal("account encoding is NOT injective — a storage-only and a code-only account collide (state root forgeable)")
	}

	// Two equal accounts still encode identically (determinism).
	a1 := &Account{Nonce: 1, Balance: big.NewInt(5), Storage: map[common.Hash]common.Hash{key: val}}
	a2 := &Account{Nonce: 1, Balance: big.NewInt(5), Storage: map[common.Hash]common.Hash{key: val}}
	if !bytes.Equal(a1.encode(), a2.encode()) {
		t.Fatal("equal accounts encode differently — determinism broken")
	}
}
