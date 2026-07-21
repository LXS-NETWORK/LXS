package vm

import "math/big"

// Memory is the EVM's linear, byte-addressed scratch space. It grows on demand
// in whole 32-byte words; growth costs gas, priced quadratically so large
// allocations are unaffordable (an unpriced expansion is a node-DoS vector).
type Memory struct {
	data []byte
}

func NewMemory() *Memory { return &Memory{} }

func (m *Memory) size() uint64 { return uint64(len(m.data)) }

// resize grows memory to at least size bytes, zero-filled. Callers must charge
// expansion gas via expansionGas first; resize itself is unconditional.
func (m *Memory) resize(size uint64) {
	if uint64(len(m.data)) < size {
		m.data = append(m.data, make([]byte, size-uint64(len(m.data)))...)
	}
}

// set writes val at offset. Memory must already be large enough.
func (m *Memory) set(offset uint64, val []byte) {
	if len(val) > 0 {
		copy(m.data[offset:offset+uint64(len(val))], val)
	}
}

// set32 writes a word as 32 big-endian bytes at offset.
func (m *Memory) set32(offset uint64, val *big.Int) {
	b := make([]byte, 32)
	val.FillBytes(b) // right-aligned, zero-padded: EVM word layout
	copy(m.data[offset:offset+32], b)
}

// get returns size bytes at offset, zero-padded if they run past the end.
func (m *Memory) get(offset, size uint64) []byte {
	out := make([]byte, size)
	if size == 0 || offset >= uint64(len(m.data)) {
		return out
	}
	copy(out, m.data[offset:min(offset+size, uint64(len(m.data)))])
	return out
}

// toWordSize is the number of 32-byte words needed to cover n bytes.
func toWordSize(n uint64) uint64 { return (n + 31) / 32 }

// memoryCost is the total gas for `words` words allocated: 3*words +
// words^2/512 (EVM). Linear for small memory, quadratic when large.
func memoryCost(words uint64) uint64 {
	return words*3 + (words*words)/512
}

// maxMemBytes bounds addressable memory. Beyond it the words*words term in
// memoryCost overflows uint64 and wraps to a small number, so the gas check
// would pass an allocation that then crashes the node in resize (a few gas ->
// terabyte make()). Refusing access past this bound keeps every cost inside
// uint64.
const maxMemBytes = uint64(1) << 32 // 4 GiB; keeps words < 2^27 so words^2 fits

// expansionGas is the gas to grow memory so [offset, offset+size) is
// addressable, or false if unpayable (address wraps uint64, or the size would
// overflow the quadratic price).
func (m *Memory) expansionGas(offset, size uint64) (uint64, bool) {
	if size == 0 {
		return 0, true
	}
	end := offset + size
	if end < offset { // uint64 overflow: an absurd offset
		return 0, false
	}
	if end > maxMemBytes { // unpayable, and would overflow the quadratic below
		return 0, false
	}
	newWords := toWordSize(end)
	oldWords := toWordSize(uint64(len(m.data)))
	if newWords <= oldWords {
		return 0, true
	}
	return memoryCost(newWords) - memoryCost(oldWords), true
}

func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
