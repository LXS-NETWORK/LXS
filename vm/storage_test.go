package vm

import (
	"errors"
	"math/big"
	"testing"

	"lxs/common"
)

// mockState is a trivial in-memory StateDB, letting the storage opcodes be
// tested without a database.
type mockState struct {
	slots  map[common.Address]map[common.Hash]common.Hash
	code   map[common.Address][]byte
	bal    map[common.Address]*big.Int
	nonces map[common.Address]uint64
	logs   []*common.Log
	snaps  []mockState // savepoints (deep copies)
}

func newMockState() *mockState {
	return &mockState{
		slots:  make(map[common.Address]map[common.Hash]common.Hash),
		code:   make(map[common.Address][]byte),
		bal:    make(map[common.Address]*big.Int),
		nonces: make(map[common.Address]uint64),
	}
}

func (m *mockState) GetNonce(addr common.Address) uint64        { return m.nonces[addr] }
func (m *mockState) SetNonce(addr common.Address, nonce uint64) { m.nonces[addr] = nonce }

func (m *mockState) GetCode(addr common.Address) []byte       { return m.code[addr] }
func (m *mockState) SetCode(addr common.Address, code []byte) { m.code[addr] = code }

func (m *mockState) GetStorage(addr common.Address, key common.Hash) common.Hash {
	return m.slots[addr][key] // zero value for an unset slot: unset == zero
}

func (m *mockState) SetStorage(addr common.Address, key, value common.Hash) {
	if m.slots[addr] == nil {
		m.slots[addr] = make(map[common.Hash]common.Hash)
	}
	if value.IsZero() {
		delete(m.slots[addr], key) // storing zero clears the slot
		return
	}
	m.slots[addr][key] = value
}

func (m *mockState) GetBalance(addr common.Address) *big.Int {
	if b, ok := m.bal[addr]; ok {
		return b
	}
	return new(big.Int)
}
func (m *mockState) AddBalance(addr common.Address, amt *big.Int) {
	m.bal[addr] = new(big.Int).Add(m.GetBalance(addr), amt)
}
func (m *mockState) SubBalance(addr common.Address, amt *big.Int) {
	m.bal[addr] = new(big.Int).Sub(m.GetBalance(addr), amt)
}

func (m *mockState) AddLog(log *common.Log) { m.logs = append(m.logs, log) }

func (m *mockState) clone() mockState {
	c := mockState{
		slots:  make(map[common.Address]map[common.Hash]common.Hash, len(m.slots)),
		code:   make(map[common.Address][]byte, len(m.code)),
		bal:    make(map[common.Address]*big.Int, len(m.bal)),
		nonces: make(map[common.Address]uint64, len(m.nonces)),
	}
	for a, n := range m.nonces {
		c.nonces[a] = n
	}
	for a, s := range m.slots {
		cs := make(map[common.Hash]common.Hash, len(s))
		for k, v := range s {
			cs[k] = v
		}
		c.slots[a] = cs
	}
	for a, code := range m.code {
		c.code[a] = append([]byte(nil), code...)
	}
	for a, b := range m.bal {
		c.bal[a] = new(big.Int).Set(b)
	}
	c.logs = append([]*common.Log(nil), m.logs...)
	return c
}

func (m *mockState) Snapshot() int {
	m.snaps = append(m.snaps, m.clone())
	return len(m.snaps) - 1
}
func (m *mockState) RevertToSnapshot(id int) {
	snap := m.snaps[id]
	m.slots, m.code, m.bal, m.nonces, m.logs = snap.slots, snap.code, snap.bal, snap.nonces, snap.logs
	m.snaps = m.snaps[:id]
}

// Store a value, run a separate call, and the value is still there. Persistence
// across executions is the difference between memory (gone at STOP) and storage
// (the contract's permanent state).
func TestStoragePersistence(t *testing.T) {
	state := newMockState()
	ctx := Context{Address: common.Address{0x01}, State: state}

	// Call 1: store[0] = 42.
	store := b(
		byte(PUSH1), 0x2a, // value 42
		byte(PUSH1), 0x00, // key 0
		byte(SSTORE),
		byte(STOP),
	)
	if r := Run(store, nil, 100_000, ctx); r.Err != nil {
		t.Fatalf("store call failed: %v", r.Err)
	}

	// Call 2 (a fresh execution, same state): load store[0] and return it.
	load := b(
		byte(PUSH1), 0x00, // key 0
		byte(SLOAD),
		byte(PUSH1), 0x00, byte(MSTORE),
		byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
	)
	r := Run(load, nil, 100_000, ctx)
	if r.Err != nil {
		t.Fatalf("load call failed: %v", r.Err)
	}
	if len(r.Ret) != 32 || r.Ret[31] != 0x2a {
		t.Fatalf("stored value did not persist across calls: got %x, want ..2a", r.Ret)
	}
}

// First write to a slot is the most expensive; overwriting is cheaper; a no-op
// cheaper still; none are free.
func TestSstoreGasTiers(t *testing.T) {
	state := newMockState()
	ctx := Context{Address: common.Address{0x02}, State: state}

	run := func(value byte) uint64 {
		code := b(byte(PUSH1), value, byte(PUSH1), 0x00, byte(SSTORE), byte(STOP))
		r := Run(code, nil, 100_000, ctx)
		if r.Err != nil {
			t.Fatalf("sstore failed: %v", r.Err)
		}
		return 100_000 - r.GasLeft
	}

	first := run(0x2a) // 0 -> 42 : SET (allocation)
	noop := run(0x2a)  // 42 -> 42 : no change
	reset := run(0x2b) // 42 -> 43 : overwrite

	if first < SstoreSetGas {
		t.Fatalf("first write cost %d, want >= %d (the allocation price)", first, SstoreSetGas)
	}
	if noop >= first || reset >= first {
		t.Fatalf("first write (%d) must be the most expensive; noop=%d reset=%d", first, noop, reset)
	}
	if noop >= reset {
		t.Fatalf("a no-op (%d) should not cost more than an overwrite (%d)", noop, reset)
	}
	if noop == 0 {
		t.Fatal("even a no-op write must cost gas — nothing about storage is free")
	}
}

// Filling the disk with fresh slots runs out of gas fast: at 20,000 gas per new
// slot, a modest budget buys only a couple.
func TestStorageSpamRunsOutOfGas(t *testing.T) {
	state := newMockState()
	ctx := Context{Address: common.Address{0x03}, State: state}

	// Three brand-new slots, but only enough gas for two.
	code := b(
		byte(PUSH1), 0x01, byte(PUSH1), 0x00, byte(SSTORE), // slot 0
		byte(PUSH1), 0x01, byte(PUSH1), 0x01, byte(SSTORE), // slot 1
		byte(PUSH1), 0x01, byte(PUSH1), 0x02, byte(SSTORE), // slot 2: runs dry here
		byte(STOP),
	)
	r := Run(code, nil, 45_000, ctx) // 2 slots = ~40k fits; a 3rd (60k) does not
	if !errors.Is(r.Err, ErrOutOfGas) {
		t.Fatalf("filling 3 fresh slots on a 45k budget should run out of gas, got %v", r.Err)
	}
}
