package types

import (
	"testing"

	"lxs/common"
)

// TestReceiptRootCommitsToLogs checks logs are consensus data: two receipts
// differing only in their logs must produce different roots. A collision would
// let a node strip or forge logs undetected by a light client.
func TestReceiptRootCommitsToLogs(t *testing.T) {
	bare := &Receipt{Status: ReceiptSuccess, GasUsed: 21000, CumulativeGasUsed: 21000}
	withLog := &Receipt{
		Status: ReceiptSuccess, GasUsed: 21000, CumulativeGasUsed: 21000,
		Logs: []*common.Log{{
			Address: common.Address{0xAB},
			Topics:  []common.Hash{{0x01}},
			Data:    []byte{0xde, 0xad},
		}},
	}

	if ReceiptRoot([]*Receipt{bare}) == ReceiptRoot([]*Receipt{withLog}) {
		t.Fatal("a receipt's logs must change its root — logs are not committed")
	}

	// Content must be committed, not just the log count: perturbing each field in
	// isolation proves address, topic and data all hash in.
	base := &Receipt{Status: ReceiptSuccess, Logs: []*common.Log{{
		Address: common.Address{0xAB}, Topics: []common.Hash{{0x01}}, Data: []byte{0xde},
	}}}
	diffAddr := &Receipt{Status: ReceiptSuccess, Logs: []*common.Log{{
		Address: common.Address{0xCD}, Topics: []common.Hash{{0x01}}, Data: []byte{0xde},
	}}}
	diffTopic := &Receipt{Status: ReceiptSuccess, Logs: []*common.Log{{
		Address: common.Address{0xAB}, Topics: []common.Hash{{0x02}}, Data: []byte{0xde},
	}}}
	diffData := &Receipt{Status: ReceiptSuccess, Logs: []*common.Log{{
		Address: common.Address{0xAB}, Topics: []common.Hash{{0x01}}, Data: []byte{0xff},
	}}}
	baseRoot := ReceiptRoot([]*Receipt{base})
	for name, r := range map[string]*Receipt{"address": diffAddr, "topic": diffTopic, "data": diffData} {
		if ReceiptRoot([]*Receipt{r}) == baseRoot {
			t.Fatalf("changing a log's %s did not change the receipt root — it is not committed", name)
		}
	}

	// And the commitment is deterministic: the same log set hashes the same.
	withLog2 := &Receipt{
		Status: ReceiptSuccess, GasUsed: 21000, CumulativeGasUsed: 21000,
		Logs: []*common.Log{{
			Address: common.Address{0xAB},
			Topics:  []common.Hash{{0x01}},
			Data:    []byte{0xde, 0xad},
		}},
	}
	if ReceiptRoot([]*Receipt{withLog}) != ReceiptRoot([]*Receipt{withLog2}) {
		t.Fatal("identical logs must produce identical roots")
	}
}
