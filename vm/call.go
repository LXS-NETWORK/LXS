package vm

import (
	"math/big"

	"lxs/common"
	"lxs/rlp"
)

// opCall runs CALL, DELEGATECALL, and STATICCALL, which share one
// implementation:
//
//	CALL:         target's code in the target's context; msg.sender = caller,
//	              value moves, storage is the target's.
//	DELEGATECALL: target's code in the CALLER's context; caller's storage, and
//	              the original msg.sender/msg.value preserved. No value moves
//	              (proxy borrowing library code).
//	STATICCALL:   target's context, no value, read-only.
//
// Failure model: a call that reverts, runs out of gas, or is refused
// (depth/balance) does not fault the caller. It pushes 0; only 1 means success.
// The caller is expected to check the result.
//
// callMode selects which call opcode opCall is running.
type callMode uint8

const (
	callRegular  callMode = iota // CALL: target's storage, value moves, msg.sender = caller
	callDelegate                 // DELEGATECALL: caller's storage, original caller/value preserved
	callStatic                   // STATICCALL: target's storage, no value, read-only
)

func (in *interp) opCall(mode callMode) error {
	st := in.stack

	gasWord, err := st.pop()
	if err != nil {
		return err
	}
	toWord, err := st.pop()
	if err != nil {
		return err
	}
	// Only a plain CALL carries a value word; DELEGATECALL and STATICCALL do not.
	value := new(big.Int)
	if mode == callRegular {
		if value, err = st.pop(); err != nil {
			return err
		}
	}
	argsOff, err := st.pop()
	if err != nil {
		return err
	}
	argsSize, err := st.pop()
	if err != nil {
		return err
	}
	retOff, err := st.pop()
	if err != nil {
		return err
	}
	retSize, err := st.pop()
	if err != nil {
		return err
	}

	// Moving value is a state change, forbidden in a static context; the ban
	// propagates, so a value-bearing CALL inside a static frame faults.
	if mode == callRegular && value.Sign() > 0 && in.ctx.Static {
		return ErrStaticStateChange
	}

	// Flat call price, plus a surcharge for moving value.
	cost := CallBaseGas
	if mode == callRegular && value.Sign() > 0 {
		cost += CallValueGas
	}
	if err := in.useGas(cost); err != nil {
		return err
	}
	// Argument and return windows are both memory the call touches; charge both.
	if err := in.chargeMem(argsOff, argsSize); err != nil {
		return err
	}
	if err := in.chargeMem(retOff, retSize); err != nil {
		return err
	}
	args := in.mem.get(argsOff.Uint64(), argsSize.Uint64())

	target := wordToAddress(toWord)

	// Gas handed to the callee: the requested amount, capped at all-but-1/64 of
	// what the caller holds (EIP-150). The reserved 1/64 leaves the caller gas
	// to handle the result and makes call depth self-limiting.
	give := in.gas - in.gas/64
	if reqGas := clampGas(gasWord); reqGas < give {
		give = reqGas
	}

	// A refused CALL leaves RETURNDATASIZE 0: clear the buffer before the early
	// refusals so a stale prior sub-call's output cannot leak into the OZ-style
	// `if (!success && returndatasize()!=0) revert(...)` bubble-up. The executed
	// path overwrites it below.
	in.returnData = nil

	// Refuse without executing (pushing 0) if at the depth ceiling or the
	// balance cannot cover the value to send.
	failFast := func() error {
		return st.push(new(big.Int)) // 0 = failure
	}
	if in.depth >= MaxCallDepth {
		return failFast()
	}
	if mode == callRegular && value.Sign() > 0 && in.ctx.State.GetBalance(in.ctx.Address).Cmp(value) < 0 {
		return failFast()
	}

	// The callee's frame. DELEGATECALL keeps the caller's address (writes land
	// in the caller's storage) and preserves caller/value; CALL and STATICCALL
	// switch to the target. STATICCALL sends no value and is read-only.
	sub := Context{
		BlockGasLimit: in.ctx.BlockGasLimit, State: in.ctx.State,
		// Block/tx environment is constant across the transaction; carried
		// through unchanged.
		Origin: in.ctx.Origin, GasPrice: in.ctx.GasPrice, Coinbase: in.ctx.Coinbase,
		BlockNumber: in.ctx.BlockNumber, Time: in.ctx.Time, Difficulty: in.ctx.Difficulty,
		ChainID: in.ctx.ChainID, BaseFee: in.ctx.BaseFee,
	}
	switch mode {
	case callDelegate:
		sub.Address, sub.Caller, sub.Value = in.ctx.Address, in.ctx.Caller, in.ctx.Value
	case callStatic:
		sub.Address, sub.Caller, sub.Value = target, in.ctx.Address, new(big.Int)
	default: // callRegular
		sub.Address, sub.Caller, sub.Value = target, in.ctx.Address, value
	}
	// Read-only propagates: a STATICCALL is static, and any call from within a
	// static frame stays static, so the ban cannot be nested away.
	sub.Static = in.ctx.Static || mode == callStatic

	// Hand over the gas, snapshot so a revert can be undone, then move value.
	in.gas -= give
	snap := in.ctx.State.Snapshot()
	if mode == callRegular && value.Sign() > 0 {
		in.ctx.State.SubBalance(in.ctx.Address, value)
		in.ctx.State.AddBalance(target, value)
	}

	// A precompile address runs native Go, not bytecode. DELEGATECALL into a
	// precompile is meaningless but Ethereum dispatches it the same way.
	var res Result
	if p := precompileFor(target); p != nil {
		res = runPrecompile(p, args, give)
	} else {
		res = execute(in.ctx.State.GetCode(target), args, give, sub, in.depth+1)
	}
	in.gas += res.GasLeft // refund whatever the callee did not spend

	// The callee's output becomes this frame's return data (readable until the
	// next call), even on failure: revert data is often the reason.
	in.returnData = res.Ret

	success := new(big.Int)
	if res.Err != nil {
		// Callee reverted or faulted: undo its value transfer and state writes,
		// report failure.
		in.ctx.State.RevertToSnapshot(snap)
	} else {
		success = big.NewInt(1)
	}

	// Return data is copied into the caller's memory window regardless of
	// success (revert data is often why it failed).
	if retSize.Sign() > 0 {
		n := retSize.Uint64()
		if uint64(len(res.Ret)) < n {
			n = uint64(len(res.Ret))
		}
		in.mem.set(retOff.Uint64(), res.Ret[:n])
	}
	return st.push(success)
}

// opCreate implements CREATE and CREATE2: deploy a contract from running code.
// The init code runs as its own frame and whatever it RETURNs becomes the new
// runtime code. Pushes the new address on success, 0 on failure.
//
// They differ only in address derivation: CREATE from the creator's nonce
// (order-dependent), CREATE2 from a caller-chosen salt and the init code
// (predictable before deployment).
func (in *interp) opCreate(create2 bool) error {
	st := in.stack
	if in.ctx.Static {
		return ErrStaticStateChange // creating a contract is a state change
	}

	value, err := st.pop()
	if err != nil {
		return err
	}
	offset, err := st.pop()
	if err != nil {
		return err
	}
	size, err := st.pop()
	if err != nil {
		return err
	}
	var salt *big.Int
	if create2 {
		if salt, err = st.pop(); err != nil {
			return err
		}
	}

	if err := in.useGas(CreateGas); err != nil {
		return err
	}
	if err := in.chargeMem(offset, size); err != nil {
		return err
	}
	if create2 {
		// CREATE2 hashes the init code to derive the address; priced per word.
		if err := in.useGas(Keccak256WordGas * toWordSize(size.Uint64())); err != nil {
			return err
		}
	}
	initCode := append([]byte(nil), in.mem.get(offset.Uint64(), size.Uint64())...)

	creator := in.ctx.Address
	// Clear RETURNDATA before the early refusals (depth/balance/collision), so a
	// refused CREATE leaves RETURNDATASIZE 0. The revert path below sets it to the
	// revert data AFTER this, so that case is preserved.
	in.returnData = nil
	fail := func() error { return st.push(new(big.Int)) } // 0 = creation failed
	if in.depth >= MaxCallDepth {
		return fail()
	}
	if value.Sign() > 0 && in.ctx.State.GetBalance(creator).Cmp(value) < 0 {
		return fail()
	}

	// The address, and the nonce a CREATE consumes from its creator.
	nonce := in.ctx.State.GetNonce(creator)
	var newAddr common.Address
	if create2 {
		newAddr = create2Address(creator, wordToHash(salt), initCode)
	} else {
		newAddr = createAddress(creator, nonce)
	}
	in.ctx.State.SetNonce(creator, nonce+1)

	// Hand the init code all-but-1/64 of the gas (EIP-150). Deducted BEFORE the collision
	// check so a collision consumes it, matching geth's ErrContractAddressCollision (which
	// burns the create's whole gas) rather than the cheaper "only CreateGas charged" path.
	give := in.gas - in.gas/64
	in.gas -= give

	// EIP-684: if the target already holds code or a nonce, the creation collides with a
	// live account; fail rather than overwrite. The creator's nonce was already consumed.
	if in.ctx.State.GetNonce(newAddr) != 0 || len(in.ctx.State.GetCode(newAddr)) > 0 {
		return fail()
	}

	snap := in.ctx.State.Snapshot()
	if value.Sign() > 0 {
		in.ctx.State.SubBalance(creator, value)
		in.ctx.State.AddBalance(newAddr, value)
	}

	sub := Context{
		BlockGasLimit: in.ctx.BlockGasLimit, State: in.ctx.State,
		Origin: in.ctx.Origin, GasPrice: in.ctx.GasPrice, Coinbase: in.ctx.Coinbase,
		BlockNumber: in.ctx.BlockNumber, Time: in.ctx.Time, Difficulty: in.ctx.Difficulty,
		ChainID: in.ctx.ChainID, BaseFee: in.ctx.BaseFee, Static: in.ctx.Static,
		Address: newAddr, Caller: creator, Value: value,
	}
	res := execute(initCode, nil, give, sub, in.depth+1)
	in.gas += res.GasLeft

	if res.Err != nil {
		// Constructor reverted or ran out of gas: undo everything and surface
		// its revert data as this frame's return data.
		in.ctx.State.RevertToSnapshot(snap)
		in.returnData = res.Ret
		return fail()
	}

	// EIP-170: reject code larger than MaxCodeSize before charging to store it.
	if uint64(len(res.Ret)) > MaxCodeSize {
		in.ctx.State.RevertToSnapshot(snap)
		return fail()
	}

	// The constructor returned the runtime code; pay to store it, per byte.
	codeGas := CreateDataGas * uint64(len(res.Ret))
	if in.gas < codeGas {
		in.ctx.State.RevertToSnapshot(snap) // can't afford the code -> creation fails
		return fail()
	}
	in.gas -= codeGas
	in.ctx.State.SetCode(newAddr, res.Ret)
	in.returnData = nil // a successful CREATE leaves no return data
	return st.push(new(big.Int).SetBytes(newAddr.Bytes()))
}

// createAddress = keccak256(rlp([creator, nonce]))[12:], Ethereum's CREATE
// derivation. Contracts here start at nonce 0, not EIP-161's 1, so a
// contract-created address need not match mainnet's.
func createAddress(creator common.Address, nonce uint64) common.Address {
	h := common.Keccak256(rlp.List(rlp.Bytes(creator[:]), rlp.Uint(nonce)))
	var a common.Address
	copy(a[:], h[12:])
	return a
}

// create2Address = keccak256(0xff ‖ creator ‖ salt ‖ keccak256(initCode))[12:].
func create2Address(creator common.Address, salt common.Hash, initCode []byte) common.Address {
	inner := common.Keccak256(initCode)
	h := common.Keccak256([]byte{0xff}, creator[:], salt[:], inner[:])
	var a common.Address
	copy(a[:], h[12:])
	return a
}

// wordToAddress takes the low 20 bytes of a 256-bit word as an address.
func wordToAddress(w *big.Int) common.Address {
	h := wordToHash(w) // 32 bytes, right-aligned
	var a common.Address
	copy(a[:], h[12:])
	return a
}

// clampGas reads a requested-gas word as uint64; an out-of-range request means
// "everything available", bounded by the 63/64 cap upstream.
func clampGas(w *big.Int) uint64 {
	if !w.IsUint64() {
		return ^uint64(0)
	}
	return w.Uint64()
}
