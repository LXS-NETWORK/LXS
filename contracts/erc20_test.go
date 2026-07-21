package contracts

import (
	"testing"

	"lxs/common"
)

// TestERC20ABIConstants pins the reference token's ABI identifiers to their
// canonical keccak-derived values. These are the bytes a real wallet or explorer
// uses to reach the contract; if they drift, every external call misses.
func TestERC20ABIConstants(t *testing.T) {
	sig := common.Keccak256([]byte("transfer(address,uint256)")).Bytes()
	want := uint32(sig[0])<<24 | uint32(sig[1])<<16 | uint32(sig[2])<<8 | uint32(sig[3])
	if TransferSelector != want {
		t.Fatalf("TransferSelector = %#x, want keccak(sig)[:4] = %#x", TransferSelector, want)
	}
	if TransferSelector != 0xa9059cbb {
		t.Fatalf("TransferSelector = %#x, want canonical 0xa9059cbb", TransferSelector)
	}
	wantTopic := "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	if got := TransferEventTopic().Hex(); got != "0x"+wantTopic {
		t.Fatalf("TransferEventTopic = %s, want the canonical Transfer topic 0x%s", got, wantTopic)
	}
}

// TestERC20InitProducesRuntime checks the constructor's CODECOPY bookkeeping:
// the init code must end with exactly the runtime bytes it claims to return.
func TestERC20InitProducesRuntime(t *testing.T) {
	runtime := ERC20Runtime()
	init := ERC20Init(common.LXS(1))
	if len(init) <= len(runtime) {
		t.Fatalf("init code (%d) must be longer than the runtime (%d) it wraps", len(init), len(runtime))
	}
	tail := init[len(init)-len(runtime):]
	for i := range runtime {
		if tail[i] != runtime[i] {
			t.Fatalf("init code does not end with the runtime at byte %d", i)
		}
	}
}
