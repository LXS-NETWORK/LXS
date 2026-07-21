// Package contracts holds reference contract bytecode the node can deploy and the
// tests can drive. The reference ERC-20 here is hand-assembled — a balances
// mapping, a transfer guarded by a revert, and a Transfer event — so every byte is
// explicit rather than an opaque solc blob. Shared by the state tests and the
// devnet demo so both exercise the same code.
package contracts

import (
	"math/big"

	"lxs/common"
	"lxs/vm"
)

// asm is a tiny label-resolving assembler so jump offsets are never hand-counted.
type asm struct {
	code    []byte
	labels  map[string]int
	patches []patchSite
}
type patchSite struct {
	pos   int
	label string
}

func newAsm() *asm { return &asm{labels: map[string]int{}} }

func (a *asm) op(o vm.OpCode) *asm { a.code = append(a.code, byte(o)); return a }
func (a *asm) raw(bs ...byte) *asm { a.code = append(a.code, bs...); return a }

// push emits PUSH<len(bs)> followed by bs (1..32 bytes).
func (a *asm) push(bs []byte) *asm {
	a.code = append(a.code, byte(vm.PUSH1)+byte(len(bs))-1)
	a.code = append(a.code, bs...)
	return a
}
func (a *asm) push1(x byte) *asm { return a.push([]byte{x}) }
func (a *asm) push4(x uint32) *asm {
	return a.push([]byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)})
}
func (a *asm) push2n(x int) *asm      { return a.push([]byte{byte(x >> 8), byte(x)}) }
func (a *asm) push32(h [32]byte) *asm { return a.push(h[:]) }

// jd marks a JUMP target: the label points at the JUMPDEST byte it emits.
func (a *asm) jd(name string) *asm { a.labels[name] = len(a.code); return a.op(vm.JUMPDEST) }

// mark records a label WITHOUT emitting a JUMPDEST — for a CODECOPY offset.
func (a *asm) mark(name string) *asm { a.labels[name] = len(a.code); return a }

// pushLabel emits PUSH2 <offset-of-label>, patched once all labels are known.
func (a *asm) pushLabel(name string) *asm {
	a.op(vm.PUSH1 + 1) // PUSH2
	a.patches = append(a.patches, patchSite{len(a.code), name})
	return a.raw(0, 0)
}

func (a *asm) done() []byte {
	for _, p := range a.patches {
		off, ok := a.labels[p.label]
		if !ok {
			panic("contracts: undefined label: " + p.label)
		}
		a.code[p.pos], a.code[p.pos+1] = byte(off>>8), byte(off)
	}
	return a.code
}

// mapSlot consumes a key on the stack and pushes keccak256(key ‖ 0) — the
// storage slot of a mapping declared at contract slot 0, Solidity's layout.
func (a *asm) mapSlot() *asm {
	return a.push1(0x00).op(vm.MSTORE). // mem[0x00] = key
						push1(0x00).push1(0x20).op(vm.MSTORE). // mem[0x20] = 0 (the mapping's own slot)
						push1(0x40).push1(0x00).op(vm.SHA3)    // keccak256(mem[0x00:0x40])
}

// TransferSelector is keccak256("transfer(address,uint256)")[:4] — the canonical
// ERC-20 transfer function selector.
const TransferSelector uint32 = 0xa9059cbb

// balanceOfSelector is keccak256("balanceOf(address)")[:4].
const balanceOfSelector uint32 = 0x70a08231

// TransferEventTopic is keccak256("Transfer(address,address,uint256)") — topic0
// of every ERC-20 Transfer log.
func TransferEventTopic() common.Hash {
	return common.Keccak256([]byte("Transfer(address,address,uint256)"))
}

// ERC20Runtime is the deployed code: a selector dispatcher over
// transfer(address,uint256) and balanceOf(address).
func ERC20Runtime() []byte {
	var sig [32]byte
	copy(sig[:], TransferEventTopic().Bytes())

	a := newAsm()

	// dispatcher: selector = calldata[0:4] = CALLDATALOAD(0) >> 224
	a.push1(0x00).op(vm.CALLDATALOAD).push1(0xE0).op(vm.SHR)
	a.op(vm.DUP1).push4(TransferSelector).op(vm.EQ).pushLabel("transfer").op(vm.JUMPI)
	a.op(vm.DUP1).push4(balanceOfSelector).op(vm.EQ).pushLabel("balanceOf").op(vm.JUMPI)
	a.push1(0x00).push1(0x00).op(vm.REVERT) // unknown selector

	// balanceOf(address): return balances[arg]
	a.jd("balanceOf").op(vm.POP)
	a.push1(0x04).op(vm.CALLDATALOAD).mapSlot().op(vm.SLOAD)
	a.push1(0x00).op(vm.MSTORE).push1(0x20).push1(0x00).op(vm.RETURN)

	// transfer(address to, uint256 amount)
	a.jd("transfer").op(vm.POP)

	// require balances[caller] >= amount (revert if amount > balance)
	a.op(vm.CALLER).mapSlot().op(vm.SLOAD)
	a.push1(0x24).op(vm.CALLDATALOAD)
	a.op(vm.GT).pushLabel("revert").op(vm.JUMPI)

	// balances[caller] -= amount
	a.op(vm.CALLER).mapSlot().op(vm.SLOAD)
	a.push1(0x24).op(vm.CALLDATALOAD)
	a.op(vm.SWAP1).op(vm.SUB)
	a.op(vm.CALLER).mapSlot().op(vm.SSTORE)

	// balances[to] += amount
	a.push1(0x04).op(vm.CALLDATALOAD).mapSlot().op(vm.SLOAD)
	a.push1(0x24).op(vm.CALLDATALOAD).op(vm.ADD)
	a.push1(0x04).op(vm.CALLDATALOAD).mapSlot().op(vm.SSTORE)

	// emit Transfer(from=caller, to, amount): LOG3, data = amount
	a.push1(0x24).op(vm.CALLDATALOAD).push1(0x00).op(vm.MSTORE)
	a.push1(0x04).op(vm.CALLDATALOAD) // topic2 = to
	a.op(vm.CALLER)                   // topic1 = from
	a.push32(sig)                     // topic0 = keccak(sig)
	a.push1(0x20).push1(0x00).op(vm.LOG0 + 3)

	// return true
	a.push1(0x01).push1(0x00).op(vm.MSTORE).push1(0x20).push1(0x00).op(vm.RETURN)

	// shared revert target
	a.jd("revert").push1(0x00).push1(0x00).op(vm.REVERT)

	return a.done()
}

// ERC20Init is the deploy bytecode: a constructor that mints the whole supply
// to the deployer, then CODECOPYs the runtime out and RETURNs it to be stored.
func ERC20Init(supply *big.Int) []byte {
	runtime := ERC20Runtime()
	a := newAsm()

	// balances[caller] = supply
	var sb [32]byte
	supply.FillBytes(sb[:])
	a.op(vm.CALLER).mapSlot()
	a.push32(sb).op(vm.SWAP1).op(vm.SSTORE)

	// CODECOPY(dest=0, offset=runtime_start, len=runtimeLen); RETURN(0, len)
	a.push2n(len(runtime)).pushLabel("runtime_start").push1(0x00).op(vm.CODECOPY)
	a.push2n(len(runtime)).push1(0x00).op(vm.RETURN)

	a.mark("runtime_start")
	return append(a.done(), runtime...)
}

// BalanceSlot is the storage slot holding balances[addr]: keccak256(pad32(addr)
// ‖ pad32(0)). Callers read a balance straight from storage with this.
func BalanceSlot(addr common.Address) common.Hash {
	var buf [64]byte
	copy(buf[12:32], addr[:]) // address right-aligned in the first word
	return common.Keccak256(buf[:])
}

// TransferCalldata ABI-encodes a call to transfer(to, amount).
func TransferCalldata(to common.Address, amount *big.Int) []byte {
	data := make([]byte, 4+32+32)
	data[0], data[1], data[2], data[3] = 0xa9, 0x05, 0x9c, 0xbb
	copy(data[4+12:4+32], to[:])
	amount.FillBytes(data[36:68])
	return data
}

// BalanceOfCalldata ABI-encodes a call to balanceOf(addr).
func BalanceOfCalldata(addr common.Address) []byte {
	data := make([]byte, 4+32)
	data[0], data[1], data[2], data[3] = 0x70, 0xa0, 0x82, 0x31
	copy(data[4+12:4+32], addr[:])
	return data
}

// ApproveCalldata ABI-encodes a call to the standard ERC-20 approve(spender, amount),
// the same selector on every ERC-20 here (UserToken, WrappedToken, a PumpCoin) — used
// so a graduating creator can let the vault pull their coin.
func ApproveCalldata(spender common.Address, amount *big.Int) []byte {
	data := make([]byte, 4+32+32)
	data[0], data[1], data[2], data[3] = 0x09, 0x5e, 0xa7, 0xb3
	copy(data[4+12:4+32], spender[:])
	amount.FillBytes(data[36:68])
	return data
}
