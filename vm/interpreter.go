// Package vm is the LXS EVM interpreter: it runs Ethereum bytecode so solc
// output executes unchanged. State access is behind the StateDB interface.
//
// Core invariant: bounded execution. Every step costs gas and gas is finite,
// so execution always halts.
package vm

import (
	"errors"
	"math/big"
)

var (
	ErrOutOfGas              = errors.New("vm: out of gas")
	ErrInvalidJump           = errors.New("vm: invalid jump destination")
	ErrInvalidOpcode         = errors.New("vm: invalid opcode")
	ErrReverted              = errors.New("vm: execution reverted")
	ErrNoState               = errors.New("vm: storage opcode with no state bound")
	ErrReturnDataOutOfBounds = errors.New("vm: return data copy out of bounds")
	ErrStaticStateChange     = errors.New("vm: state change inside a static call")
	ErrMaxCodeSize           = errors.New("vm: deployed code exceeds the max size (EIP-170)")
	tt256                    = new(big.Int).Lsh(big.NewInt(1), 256)
	big1                     = big.NewInt(1)
	big0                     = big.NewInt(0)
)

// Result is the outcome of running bytecode.
type Result struct {
	Ret     []byte // RETURN/REVERT data (nil on STOP)
	GasLeft uint64
	Err     error // nil on success; ErrReverted on REVERT; a hard error otherwise
}

// Run executes code with the given call input and gas budget.
//
// Gas policy: REVERT refunds unspent gas; any other failure (out of gas, bad
// jump, stack fault, invalid opcode) consumes all of it.
func Run(code, input []byte, gas uint64, ctx Context) Result {
	return execute(code, input, gas, ctx, 0)
}

// execute runs one call frame at the given depth. CALL/DELEGATECALL recurse
// with depth+1, bounded by MaxCallDepth.
func execute(code, input []byte, gas uint64, ctx Context, depth int) Result {
	in := &interp{
		code:  code,
		input: input,
		stack: NewStack(),
		mem:   NewMemory(),
		gas:   gas,
		jumps: analyseJumpdests(code),
		ctx:   ctx,
		depth: depth,
	}
	ret, err := in.run()
	switch {
	case err == nil:
		return Result{Ret: ret, GasLeft: in.gas}
	case errors.Is(err, ErrReverted):
		return Result{Ret: ret, GasLeft: in.gas, Err: err}
	default:
		return Result{GasLeft: 0, Err: err} // hard failure burns all gas
	}
}

type interp struct {
	code   []byte
	input  []byte
	stack  *Stack
	mem    *Memory
	pc     uint64
	gas    uint64
	jumps  []bool // jumps[i] == true if byte i is a valid JUMPDEST
	jumped bool   // set by JUMP/JUMPI so the loop does not also advance pc
	ctx    Context
	depth  int // frame depth (0 = top level)
	// returnData holds the last sub-call's output for RETURNDATASIZE/COPY.
	// Overwritten only by the next CALL, so it outlives the call that set it.
	returnData []byte
}

// useGas subtracts cost, or reports out-of-gas without going negative.
func (in *interp) useGas(cost uint64) error {
	if in.gas < cost {
		in.gas = 0
		return ErrOutOfGas
	}
	in.gas -= cost
	return nil
}

func (in *interp) run() ([]byte, error) {
	for in.pc < uint64(len(in.code)) {
		op := OpCode(in.code[in.pc])

		if err := in.useGas(gasCost[op]); err != nil {
			return nil, err
		}

		switch {
		case op == STOP:
			return nil, nil
		case op >= PUSH1 && op <= PUSH32:
			if err := in.doPush(int(op-PUSH1) + 1); err != nil {
				return nil, err
			}
			continue
		case op >= DUP1 && op <= DUP16:
			if err := in.stack.dup(int(op-DUP1) + 1); err != nil {
				return nil, err
			}
		case op >= SWAP1 && op <= SWAP16:
			if err := in.stack.swap(int(op-SWAP1) + 1); err != nil {
				return nil, err
			}
		default:
			done, ret, err := in.exec(op)
			if err != nil {
				return ret, err
			}
			if done {
				return ret, nil
			}
			if in.jumped {
				in.jumped = false
				continue
			}
		}
		in.pc++
	}
	return nil, nil // ran off the end == implicit STOP
}
