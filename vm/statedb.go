package vm

import (
	"math/big"

	"lxs/common"
)

// StateDB is the state the VM reads and writes, behind an interface so the VM
// does not depend on the storage backend.
//
// A read of an unset slot returns the zero hash: unset == zero, as for
// balances. Load-bearing for gas — clearing a slot to zero stops paying for it.
type StateDB interface {
	GetStorage(addr common.Address, key common.Hash) common.Hash
	SetStorage(addr common.Address, key, value common.Hash)
	GetCode(addr common.Address) []byte
	SetCode(addr common.Address, code []byte)

	// Nonce: CREATE derives its address from keccak(rlp([creator, nonce])) and
	// consumes a nonce from the creator.
	GetNonce(addr common.Address) uint64
	SetNonce(addr common.Address, nonce uint64)

	// Balances, for CALL's value transfer.
	GetBalance(addr common.Address) *big.Int
	AddBalance(addr common.Address, amount *big.Int)
	SubBalance(addr common.Address, amount *big.Int)

	// Snapshot/RevertToSnapshot bound a call frame's writes: a reverting
	// sub-call must undo everything (value moved, storage written) while the
	// caller continues. Snapshot returns an opaque id; reverting restores state
	// to that point.
	Snapshot() int
	RevertToSnapshot(id int)

	// AddLog records a LOG0..LOG4 event. Logs must vanish if the frame reverts,
	// so they ride the same snapshot stack as storage.
	AddLog(log *common.Log)
}

// MaxCallDepth is the ceiling on nested calls (Ethereum's 1024). A CALL at the
// ceiling fails without executing, bounding recursion so it cannot overflow the
// host stack. It bounds depth, not reentrancy.
const MaxCallDepth = 1024

// MaxCodeSize caps deployed contract code (EIP-170, Spurious Dragon). Code lives
// on every node forever, so an unbounded contract is a permanent burden; a create
// that returns more than this fails. Mainnet-exact so a contract that deploys here
// deploys there.
const MaxCodeSize = 24576

// Context is the call frame an execution runs in. The environment opcodes
// (ADDRESS, CALLER, CALLVALUE, GASLIMIT) read from here.
//
// State may be nil for a pure arithmetic run; storage and code opcodes then
// fault.
type Context struct {
	Address       common.Address // the account whose code is running (self)
	Caller        common.Address // who invoked this frame (msg.sender)
	Value         *big.Int       // wei sent with the call (msg.value)
	BlockGasLimit uint64         // the block's gas limit, for the GASLIMIT opcode
	State         StateDB

	// Block/tx environment: constant for the whole transaction and inherited
	// unchanged by every sub-call.
	Origin      common.Address // tx.origin: the EOA that started the tx (never a contract)
	GasPrice    *big.Int       // the transaction's gas price
	Coinbase    common.Address // the block's miner/proposer
	BlockNumber uint64         // the block's height
	Time        uint64         // the block's timestamp, in SECONDS (Ethereum's unit)
	Difficulty  *big.Int       // the block's mining difficulty
	ChainID     uint64         // the chain id
	BaseFee     *big.Int       // EIP-1559 base fee (always 0 here)

	// Static marks a read-only frame (STATICCALL). Any state change (SSTORE,
	// LOG, value-bearing CALL) faults; the flag propagates into every sub-call.
	Static bool
}

// wordToHash renders a 256-bit stack word as a 32-byte key or value. The word
// is already reduced mod 2^256, so it always fits.
func wordToHash(w *big.Int) common.Hash {
	var h common.Hash
	w.FillBytes(h[:])
	return h
}
