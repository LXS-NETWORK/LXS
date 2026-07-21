package vm

import "lxs/common"

// OpCode is a single EVM instruction byte. Values match Ethereum's exactly so
// solc output runs unchanged.
type OpCode byte

const (
	STOP OpCode = 0x00
	ADD  OpCode = 0x01
	MUL  OpCode = 0x02
	SUB  OpCode = 0x03
	DIV  OpCode = 0x04
	SDIV OpCode = 0x05 // signed division (two's-complement)
	MOD  OpCode = 0x06
	SMOD OpCode = 0x07 // signed modulo
	// ADDMOD/MULMOD compute (a op b) mod n in full precision, so the
	// intermediate never wraps at 2^256 (unlike a plain ADD-then-MOD).
	ADDMOD     OpCode = 0x08
	MULMOD     OpCode = 0x09
	EXP        OpCode = 0x0a
	SIGNEXTEND OpCode = 0x0b // sign-extend a value from a given byte width

	LT     OpCode = 0x10
	GT     OpCode = 0x11
	SLT    OpCode = 0x12 // signed less-than (two's-complement)
	SGT    OpCode = 0x13 // signed greater-than
	EQ     OpCode = 0x14
	ISZERO OpCode = 0x15
	AND    OpCode = 0x16
	OR     OpCode = 0x17
	XOR    OpCode = 0x18
	NOT    OpCode = 0x19
	BYTE   OpCode = 0x1a // extract the i-th big-endian byte
	SHL    OpCode = 0x1b
	SHR    OpCode = 0x1c
	SAR    OpCode = 0x1d // arithmetic (sign-propagating) shift right

	// SHA3 (KECCAK256) hashes a memory region. Solidity mappings depend on it:
	// balances[addr] lives at keccak256(addr ‖ baseSlot), computed on-chain here.
	SHA3 OpCode = 0x20

	// Environment / call-frame opcodes: where the contract runs and who called.
	ADDRESS OpCode = 0x30
	// Account-inspection opcodes. EXTCODESIZE backs Solidity's isContract check.
	BALANCE     OpCode = 0x31
	EXTCODESIZE OpCode = 0x3b
	// EXTCODECOPY copies another account's code into memory (minimal proxies,
	// EIP-1167). Missing it, such contracts halt on an unknown opcode.
	EXTCODECOPY  OpCode = 0x3c
	EXTCODEHASH  OpCode = 0x3f
	SELFBALANCE  OpCode = 0x47
	ORIGIN       OpCode = 0x32
	CALLER       OpCode = 0x33
	CALLVALUE    OpCode = 0x34
	CALLDATALOAD OpCode = 0x35
	CALLDATASIZE OpCode = 0x36
	CALLDATACOPY OpCode = 0x37
	GASPRICE     OpCode = 0x3a
	// Block-context opcodes. Deadline logic uses TIMESTAMP/NUMBER; CHAINID
	// underpins EIP-2612 permit signatures.
	BLOCKHASH  OpCode = 0x40
	COINBASE   OpCode = 0x41
	TIMESTAMP  OpCode = 0x42
	NUMBER     OpCode = 0x43
	DIFFICULTY OpCode = 0x44
	CHAINID    OpCode = 0x46
	BASEFEE    OpCode = 0x48
	// CODESIZE/CODECOPY read the running code. A constructor uses CODECOPY to
	// lift its runtime half into memory and RETURN it: how solc deploys.
	CODESIZE OpCode = 0x38
	CODECOPY OpCode = 0x39
	// RETURNDATASIZE/COPY expose the last sub-call's output. Unlike calldata, an
	// out-of-range RETURNDATACOPY faults instead of zero-padding.
	RETURNDATASIZE OpCode = 0x3d
	RETURNDATACOPY OpCode = 0x3e
	GASLIMIT       OpCode = 0x45

	POP      OpCode = 0x50
	MLOAD    OpCode = 0x51
	MSTORE   OpCode = 0x52
	MSTORE8  OpCode = 0x53
	SLOAD    OpCode = 0x54
	SSTORE   OpCode = 0x55
	JUMP     OpCode = 0x56
	JUMPI    OpCode = 0x57
	PC       OpCode = 0x58
	MSIZE    OpCode = 0x59
	GAS      OpCode = 0x5a
	JUMPDEST OpCode = 0x5b
	// MCOPY (Cancun) and PUSH0 (Shanghai): modern solc emits these by default
	// (PUSH0 for `PUSH1 0x00`, MCOPY for memory-to-memory copies).
	MCOPY OpCode = 0x5e
	PUSH0 OpCode = 0x5f

	// PUSH1..PUSH32 are 0x60..0x7f: push the next N code bytes as a word.
	PUSH1  OpCode = 0x60
	PUSH32 OpCode = 0x7f

	// DUP1..DUP16 are 0x80..0x8f; SWAP1..SWAP16 are 0x90..0x9f.
	DUP1   OpCode = 0x80
	DUP16  OpCode = 0x8f
	SWAP1  OpCode = 0x90
	SWAP16 OpCode = 0x9f

	// Events: LOG0..LOG4 differ only in indexed-topic count. An ERC-20 transfer
	// emits LOG3 (Transfer + from + to).
	LOG0 OpCode = 0xa0
	LOG4 OpCode = 0xa4

	// Contract creation from within the VM: factory patterns.
	CREATE  OpCode = 0xf0
	CREATE2 OpCode = 0xf5

	// Cross-contract calls.
	CALL         OpCode = 0xf1
	RETURN       OpCode = 0xf3
	DELEGATECALL OpCode = 0xf4
	STATICCALL   OpCode = 0xfa
	REVERT       OpCode = 0xfd
	INVALID      OpCode = 0xfe
)

// Log gas: flat base per LOG, surcharge per indexed topic, per-byte on data.
// Events are permanent history, so a large one is not cheap.
const (
	LogGas      uint64 = 375
	LogTopicGas uint64 = 375
	LogDataGas  uint64 = 8
)

// Call gas. CallBaseGas is the flat price of a call; CallValueGas is the
// surcharge for moving value (Ethereum). The gas handed to the callee is
// separate: deducted from the caller and refunded if unspent.
const (
	CallBaseGas  uint64 = 700
	CallValueGas uint64 = 9000
)

// SHA3 gas: flat base plus a per-word surcharge on the hashed length, tying
// cost to work so a contract cannot keccak megabytes for a flat fee.
const (
	Keccak256Gas     uint64 = 30
	Keccak256WordGas uint64 = 6
)

// Contract-creation gas. CreateGas is the flat price of a CREATE; CreateDataGas
// is charged per byte of returned runtime code, which lives on every node
// forever (Ethereum's 200/byte).
const (
	CreateGas     uint64 = 32000
	CreateDataGas uint64 = 200
)

// isPush reports whether op is PUSH1..PUSH32 and how many data bytes it
// consumes. Jumpdest analysis uses this to skip push data, which must never be
// mistaken for a JUMPDEST.
func isPush(op OpCode) (n int, ok bool) {
	if op >= PUSH1 && op <= PUSH32 {
		return int(op-PUSH1) + 1, true
	}
	return 0, false
}

// gasCost is the static gas per opcode (EVM schedule). Dynamic costs (memory
// expansion, EXP exponent) are added at execution time. Zero means free (STOP)
// or entirely dynamic (RETURN/REVERT pay memory only; SSTORE/CALL charge
// themselves).
//
// Istanbul schedule (pre-Berlin, before EIP-2929 warm/cold access lists).
// Pinned by TestGasScheduleIsIstanbul: a drifted cost is a consensus bug (nodes
// disagree on gasUsed and the receipt root) and a DoS vector (underpriced
// opcode, cf. EIP-150).
//
// Deliberate deviations from mainnet:
//   - No EIP-2929 warm/cold accounting: SLOAD is a flat 800. Adopting 2100
//     without the access-list machinery would match no real fork.
//   - CALL models neither the 2300-gas value-transfer stipend nor the 25000
//     new-account surcharge. Neither matters for the contracts run so far.
var gasCost = func() [256]uint64 {
	var g [256]uint64
	// GasQuickStep(2), GasFastestStep(3), GasFastStep(5), etc. — EVM names.
	const (
		quick   = 2
		fastest = 3
		fast    = 5
		mid     = 8
		slow    = 10
	)
	for op := PUSH1; op <= PUSH32; op++ {
		g[op] = fastest
	}
	for op := DUP1; op <= DUP16; op++ {
		g[op] = fastest
	}
	for op := SWAP1; op <= SWAP16; op++ {
		g[op] = fastest
	}
	g[ADD], g[SUB], g[NOT], g[LT], g[GT], g[EQ], g[ISZERO] = fastest, fastest, fastest, fastest, fastest, fastest, fastest
	g[SLT], g[SGT] = fastest, fastest
	// Account inspection reaches into another account's record, dearer than a
	// stack op (Istanbul EIP-1884). SELFBALANCE is cheap (reads self).
	g[BALANCE], g[EXTCODESIZE], g[EXTCODEHASH] = 700, 700, 700
	// Same base as its siblings; per-word copy and memory expansion are charged
	// dynamically in exec.go, like CODECOPY.
	g[EXTCODECOPY] = 700
	g[SELFBALANCE] = fast
	g[AND], g[OR], g[XOR], g[SHL], g[SHR], g[SAR], g[BYTE] = fastest, fastest, fastest, fastest, fastest, fastest, fastest
	g[MUL], g[DIV], g[MOD], g[SDIV], g[SMOD], g[SIGNEXTEND] = fast, fast, fast, fast, fast, fast
	g[ADDMOD], g[MULMOD] = mid, mid
	g[EXP] = slow          // plus dynamic per exponent byte
	g[SHA3] = Keccak256Gas // plus dynamic per-word + memory expansion
	g[POP] = quick
	g[MLOAD], g[MSTORE], g[MSTORE8] = fastest, fastest, fastest // plus memory expansion
	g[PC], g[MSIZE], g[GAS] = quick, quick, quick
	g[PUSH0] = quick   // Shanghai: push a zero
	g[MCOPY] = fastest // Cancun: mem->mem copy, plus per-word + expansion
	g[ADDRESS], g[CALLER], g[CALLVALUE], g[CALLDATASIZE], g[GASLIMIT] = quick, quick, quick, quick, quick
	g[ORIGIN], g[GASPRICE], g[COINBASE], g[TIMESTAMP] = quick, quick, quick, quick
	g[NUMBER], g[DIFFICULTY], g[CHAINID], g[BASEFEE] = quick, quick, quick, quick
	g[BLOCKHASH] = 20 // G_blockhash: touches recent history
	g[CALLDATALOAD] = fastest
	g[CALLDATACOPY] = fastest // plus per-word copy + memory expansion, charged dynamically
	g[CODESIZE] = quick
	g[CODECOPY] = fastest       // plus per-word copy + memory expansion, charged dynamically
	g[RETURNDATASIZE] = quick   // reading the size of the last call's return
	g[RETURNDATACOPY] = fastest // plus per-word copy + memory expansion, charged dynamically
	// LOG0..LOG4 static is 0: whole cost is dynamic (base + per-topic +
	// per-byte), see exec.go.
	g[JUMP] = mid
	g[JUMPI] = slow
	g[JUMPDEST] = 1
	g[SLOAD] = SloadGas // reads are flat
	// SSTORE static is 0: whole cost is dynamic, depending on the slot's current
	// value (see sstoreGas). STOP/RETURN/REVERT: 0 static (RETURN/REVERT pay
	// memory expansion only).
	return g
}()

// Storage gas: the most expensive operation, deliberately. A slot is a
// permanent row every full node keeps forever. Pre-EIP-2929 numbers: a first
// write costs SstoreSet, changing a slot costs SstoreReset, a no-op still costs
// SstoreNoop (the node read the slot).
//
// At SstoreSet a transaction creates at most GasLimit/20,000 new slots before
// running dry, so disk bloat is gas-bound.
const (
	SloadGas       uint64 = 800
	SstoreSetGas   uint64 = 20000 // 0 -> nonzero: a new slot, disk allocation
	SstoreResetGas uint64 = 5000  // nonzero -> different (or -> 0): overwrite
	SstoreNoopGas  uint64 = 800   // unchanged: still not free
)

// sstoreGas prices a write from cur to next.
func sstoreGas(cur, next common.Hash) uint64 {
	switch {
	case cur == next:
		return SstoreNoopGas
	case cur.IsZero(): // 0 -> nonzero
		return SstoreSetGas
	default: // nonzero -> different value, or nonzero -> 0
		return SstoreResetGas
	}
}
