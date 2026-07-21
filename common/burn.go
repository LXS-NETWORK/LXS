package common

// BurnAddress is the protocol's sink for destroyed value: 0x…dEaD. Defined in
// the lowest package so every caller names the same address; a drifted copy
// would be a consensus split. The state transition recognises a send here as a
// burn, folding the value into the consensus-tracked total rather than crediting
// any account, so the address never holds a spendable balance.
var BurnAddress = mustBurnAddress()

func mustBurnAddress() Address {
	a, err := AddressFromHex("000000000000000000000000000000000000dEaD")
	if err != nil {
		panic("common: bad BurnAddress literal: " + err.Error())
	}
	return a
}
