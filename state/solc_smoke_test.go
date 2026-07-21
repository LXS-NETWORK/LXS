package state

import (
	"encoding/hex"
	"math/big"
	"testing"

	"lxs/common"
	"lxs/types"
)

// TestVMRunsSolcIstanbulBytecode deploys the bytecode of a trivial contract
// compiled by solc 0.8.26 with evmVersion=istanbul (the version this VM's gas
// schedule is pinned to, which keeps solc from emitting PUSH0 or other
// post-Istanbul opcodes the VM lacks), then exercises a setter and getter end to
// end. Passing means arbitrary istanbul-compiled Solidity runs here.
//
// contract Ping { uint256 public value; function set(uint256 v) external { value = v; } }
func TestVMRunsSolcIstanbulBytecode(t *testing.T) {
	const deployHex = "6080604052348015600f57600080fd5b5060b180601d6000396000f3fe6080604052348015600f57600080fd5b506004361060325760003560e01c80633fa4f24514603757806360fe47b1146051575b600080fd5b603f60005481565b60405190815260200160405180910390f35b6061605c3660046063565b600055565b005b600060208284031215607457600080fd5b503591905056fea26469706673582212205a6445b35be4e4d4e7385773f12939eb4131153c931fca1801ee5bfa8b6ec96064736f6c634300081a0033"

	deploy, err := hex.DecodeString(deployHex)
	if err != nil {
		t.Fatal(err)
	}

	dev := key(t)
	s := New()
	s.Credit(dev.Address(), common.LXS(100))

	apply := func(nonce uint64, to *common.Address, data []byte) uint64 {
		t.Helper()
		tx := &types.Transaction{Type: types.TxTypeTransfer, ChainID: chainID, Nonce: nonce, To: to,
			Value: big.NewInt(0), GasLimit: 5_000_000, GasPrice: big.NewInt(1), Data: data}
		if err := tx.Sign(dev); err != nil {
			t.Fatal(err)
		}
		_, st, _, err := ApplyTx(s, tx, common.Address{}, 30_000_000)
		if err != nil {
			t.Fatalf("tx errored the block: %v", err)
		}
		return st
	}

	if st := apply(0, nil, deploy); st != types.ReceiptSuccess {
		t.Fatal("deploying solc bytecode failed — the VM cannot run istanbul-compiled Solidity")
	}
	ping := CreateAddress(dev.Address(), 0)

	// set(uint256 = 42): selector 60fe47b1 ‖ pad32(42)
	setData := append([]byte{0x60, 0xfe, 0x47, 0xb1}, leftPad32(big.NewInt(42))...)
	if st := apply(1, &ping, setData); st != types.ReceiptSuccess {
		t.Fatal("calling set(42) on the solc contract failed")
	}

	// value(): selector 3fa4f245, read via eth_call
	out, err := Call(s, dev.Address(), ping, []byte{0x3f, 0xa4, 0xf2, 0x45}, 5_000_000)
	if err != nil {
		t.Fatalf("value() call failed: %v", err)
	}
	if got := new(big.Int).SetBytes(out); got.Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("value() = %s, want 42 — solc state write/read did not round-trip", got)
	}
}

func leftPad32(n *big.Int) []byte {
	var b [32]byte
	n.FillBytes(b[:])
	return b[:]
}
