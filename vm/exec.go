package vm

import (
	"math/big"

	"lxs/common"
)

// analyseJumpdests marks every valid JUMPDEST byte. A 0x5b inside PUSH data is
// a data byte, not a destination; skipping push data here stops a jump into the
// middle of a constant.
func analyseJumpdests(code []byte) []bool {
	dests := make([]bool, len(code))
	for i := 0; i < len(code); {
		op := OpCode(code[i])
		if op == JUMPDEST {
			dests[i] = true
			i++
			continue
		}
		if n, ok := isPush(op); ok {
			i += n + 1 // skip the opcode AND its data bytes
			continue
		}
		i++
	}
	return dests
}

// doPush reads the next n code bytes as a big-endian word and pushes it,
// advancing pc past the opcode and its data. Bytes past end-of-code are zero
// (EVM truncated-PUSH behaviour).
func (in *interp) doPush(n int) error {
	start := in.pc + 1
	end := start + uint64(n)
	buf := make([]byte, n)
	for i := uint64(0); i < uint64(n) && start+i < uint64(len(in.code)); i++ {
		buf[i] = in.code[start+i]
	}
	in.pc = end
	return in.stack.push(new(big.Int).SetBytes(buf))
}

// exec runs one non-push, non-dup, non-swap opcode. It returns done=true when
// execution should stop (STOP/RETURN/REVERT), the return data, and any error.
func (in *interp) exec(op OpCode) (done bool, ret []byte, err error) {
	st := in.stack

	pop2 := func() (a, b *big.Int, err error) {
		if a, err = st.pop(); err != nil {
			return
		}
		b, err = st.pop()
		return
	}

	switch op {
	case ADD, MUL, SUB, DIV, SDIV, MOD, SMOD, SIGNEXTEND, EXP,
		LT, GT, SLT, SGT, EQ, AND, OR, XOR, BYTE, SHL, SHR, SAR:
		a, b, e := pop2()
		if e != nil {
			return false, nil, e
		}
		return false, nil, in.binop(op, a, b)

	case ADDMOD, MULMOD:
		// (a op b) mod n in full precision (no 2^256 wrap before the reduce).
		// n == 0 yields 0, per the EVM div/mod-by-zero convention.
		a, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		b, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		n, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		z := new(big.Int)
		if n.Sign() != 0 {
			if op == ADDMOD {
				z.Add(a, b)
			} else {
				z.Mul(a, b)
			}
			z.Mod(z, n)
		}
		return false, nil, st.push(z)

	case SHA3:
		// keccak256 over a memory window: charge expansion, then per-word on the
		// length, then hash.
		off, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		size, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if e := in.chargeMem(off, size); e != nil {
			return false, nil, e
		}
		if e := in.useGas(Keccak256WordGas * toWordSize(size.Uint64())); e != nil {
			return false, nil, e
		}
		h := common.Keccak256(in.mem.get(off.Uint64(), size.Uint64()))
		return false, nil, st.push(new(big.Int).SetBytes(h.Bytes()))

	case ISZERO:
		a, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if a.Sign() == 0 {
			return false, nil, st.push(new(big.Int).Set(big1))
		}
		return false, nil, st.push(new(big.Int))

	case NOT:
		a, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		// bitwise NOT within 256 bits = mask XOR a.
		return false, nil, st.push(wrap(new(big.Int).Xor(a, tt256m1)))

	case POP:
		_, e := st.pop()
		return false, nil, e

	case MLOAD:
		off, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if e := in.chargeMem(off, big.NewInt(32)); e != nil {
			return false, nil, e
		}
		o := off.Uint64()
		return false, nil, st.push(new(big.Int).SetBytes(in.mem.get(o, 32)))

	case MSTORE:
		off, val, e := pop2()
		if e != nil {
			return false, nil, e
		}
		if e := in.chargeMem(off, big.NewInt(32)); e != nil {
			return false, nil, e
		}
		in.mem.set32(off.Uint64(), val)
		return false, nil, nil

	case MSTORE8:
		off, val, e := pop2()
		if e != nil {
			return false, nil, e
		}
		if e := in.chargeMem(off, big1); e != nil {
			return false, nil, e
		}
		in.mem.set(off.Uint64(), []byte{byte(val.Uint64() & 0xff)})
		return false, nil, nil

	case SLOAD:
		key, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if in.ctx.State == nil {
			return false, nil, ErrNoState
		}
		v := in.ctx.State.GetStorage(in.ctx.Address, wordToHash(key))
		return false, nil, st.push(new(big.Int).SetBytes(v.Bytes()))

	case SSTORE:
		if in.ctx.Static {
			return false, nil, ErrStaticStateChange // no writes in a read-only frame
		}
		key, val, e := pop2()
		if e != nil {
			return false, nil, e
		}
		if in.ctx.State == nil {
			return false, nil, ErrNoState
		}
		kh, vh := wordToHash(key), wordToHash(val)
		// Price the write against the slot's current value (first write vs
		// overwrite vs no-op differ), then commit.
		cur := in.ctx.State.GetStorage(in.ctx.Address, kh)
		if e := in.useGas(sstoreGas(cur, vh)); e != nil {
			return false, nil, e
		}
		in.ctx.State.SetStorage(in.ctx.Address, kh, vh)
		return false, nil, nil

	case ADDRESS:
		return false, nil, st.push(new(big.Int).SetBytes(in.ctx.Address.Bytes()))

	case BALANCE:
		addr, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if in.ctx.State == nil {
			return false, nil, ErrNoState
		}
		return false, nil, st.push(new(big.Int).Set(in.ctx.State.GetBalance(wordToAddress(addr))))

	case SELFBALANCE:
		if in.ctx.State == nil {
			return false, nil, ErrNoState
		}
		return false, nil, st.push(new(big.Int).Set(in.ctx.State.GetBalance(in.ctx.Address)))

	case EXTCODESIZE:
		addr, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if in.ctx.State == nil {
			return false, nil, ErrNoState
		}
		return false, nil, st.push(new(big.Int).SetUint64(uint64(len(in.ctx.State.GetCode(wordToAddress(addr))))))

	case EXTCODEHASH:
		addr, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if in.ctx.State == nil {
			return false, nil, ErrNoState
		}
		// EIP-1052: a coded account hashes its code; an EXISTING (funded or nonced) but
		// code-less account hashes to keccak256("") (the empty-code hash); only a truly
		// non-existent account returns 0. Contracts distinguish an EOA from an untouched
		// address via extcodehash(a) == keccak256(""), so conflating the two diverges them.
		a := wordToAddress(addr)
		code := in.ctx.State.GetCode(a)
		if len(code) > 0 {
			return false, nil, st.push(new(big.Int).SetBytes(common.Keccak256(code).Bytes()))
		}
		if in.ctx.State.GetBalance(a).Sign() != 0 || in.ctx.State.GetNonce(a) != 0 {
			return false, nil, st.push(new(big.Int).SetBytes(common.Keccak256(nil).Bytes()))
		}
		return false, nil, st.push(new(big.Int)) // non-existent
	case CALLER:
		return false, nil, st.push(new(big.Int).SetBytes(in.ctx.Caller.Bytes()))
	case ORIGIN:
		return false, nil, st.push(new(big.Int).SetBytes(in.ctx.Origin.Bytes()))
	case GASPRICE:
		return false, nil, st.push(bigOrZero(in.ctx.GasPrice))
	case COINBASE:
		return false, nil, st.push(new(big.Int).SetBytes(in.ctx.Coinbase.Bytes()))
	case TIMESTAMP:
		return false, nil, st.push(new(big.Int).SetUint64(in.ctx.Time))
	case NUMBER:
		return false, nil, st.push(new(big.Int).SetUint64(in.ctx.BlockNumber))
	case DIFFICULTY:
		return false, nil, st.push(bigOrZero(in.ctx.Difficulty))
	case CHAINID:
		return false, nil, st.push(new(big.Int).SetUint64(in.ctx.ChainID))
	case BASEFEE:
		return false, nil, st.push(bigOrZero(in.ctx.BaseFee))
	case BLOCKHASH:
		// Return 0. A faithful BLOCKHASH needs the same recent-history lookup
		// on producer and validators or their state roots diverge; a constant
		// is the safe placeholder until that is threaded through.
		if _, e := st.pop(); e != nil {
			return false, nil, e
		}
		return false, nil, st.push(new(big.Int))
	case CALLVALUE:
		v := in.ctx.Value
		if v == nil {
			v = big0
		}
		return false, nil, st.push(new(big.Int).Set(v))
	case GASLIMIT:
		return false, nil, st.push(new(big.Int).SetUint64(in.ctx.BlockGasLimit))
	case CALLDATASIZE:
		return false, nil, st.push(new(big.Int).SetUint64(uint64(len(in.input))))

	case CALLDATALOAD:
		off, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		return false, nil, st.push(new(big.Int).SetBytes(getData(in.input, off, 32)))

	case CALLDATACOPY:
		memOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		dataOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		length, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if e := in.chargeMem(memOff, length); e != nil {
			return false, nil, e
		}
		// 3 gas per word copied, on top of expansion, so a contract cannot
		// copy megabytes for a flat fee. length fits uint64 (chargeMem
		// refused otherwise).
		if e := in.useGas(3 * toWordSize(length.Uint64())); e != nil {
			return false, nil, e
		}
		in.mem.set(memOff.Uint64(), getData(in.input, dataOff, length.Uint64()))
		return false, nil, nil

	case CODESIZE:
		return false, nil, st.push(new(big.Int).SetUint64(uint64(len(in.code))))

	case CODECOPY:
		// Like CALLDATACOPY, but the source is the contract's own code. The
		// constructor workhorse: copy runtime bytes out of the init code and
		// RETURN them.
		memOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		codeOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		length, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if e := in.chargeMem(memOff, length); e != nil {
			return false, nil, e
		}
		if e := in.useGas(3 * toWordSize(length.Uint64())); e != nil {
			return false, nil, e
		}
		in.mem.set(memOff.Uint64(), getData(in.code, codeOff, length.Uint64()))
		return false, nil, nil

	case EXTCODECOPY:
		// Like CODECOPY, but the source is another account's code, so the address
		// is popped first. getData zero-pads a read past end-of-code (matching
		// EXTCODESIZE's length 0), so copying from an EOA yields zeros, not a
		// fault.
		addr, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		memOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		codeOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		length, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if in.ctx.State == nil {
			return false, nil, ErrNoState
		}
		if e := in.chargeMem(memOff, length); e != nil {
			return false, nil, e
		}
		if e := in.useGas(3 * toWordSize(length.Uint64())); e != nil {
			return false, nil, e
		}
		code := in.ctx.State.GetCode(wordToAddress(addr))
		in.mem.set(memOff.Uint64(), getData(code, codeOff, length.Uint64()))
		return false, nil, nil

	case JUMP:
		dest, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		return false, nil, in.jump(dest)

	case JUMPI:
		dest, cond, e := pop2()
		if e != nil {
			return false, nil, e
		}
		if cond.Sign() != 0 {
			return false, nil, in.jump(dest)
		}
		return false, nil, nil // fall through; pc advances normally

	case JUMPDEST:
		return false, nil, nil // a no-op marker

	case PUSH0:
		return false, nil, st.push(new(big.Int))

	case MCOPY:
		// Memory-to-memory copy: both source and destination touch memory, so
		// both are charged expansion, plus 3 gas per word.
		destOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		srcOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		length, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		if e := in.chargeMem(destOff, length); e != nil {
			return false, nil, e
		}
		if e := in.chargeMem(srcOff, length); e != nil {
			return false, nil, e
		}
		if e := in.useGas(3 * toWordSize(length.Uint64())); e != nil {
			return false, nil, e
		}
		// mem.get returns a fresh copy, so an overlapping copy is still correct.
		in.mem.set(destOff.Uint64(), in.mem.get(srcOff.Uint64(), length.Uint64()))
		return false, nil, nil

	case PC:
		return false, nil, st.push(new(big.Int).SetUint64(in.pc))
	case MSIZE:
		return false, nil, st.push(new(big.Int).SetUint64(in.mem.size()))
	case GAS:
		return false, nil, st.push(new(big.Int).SetUint64(in.gas))

	case RETURNDATASIZE:
		return false, nil, st.push(new(big.Int).SetUint64(uint64(len(in.returnData))))

	case RETURNDATACOPY:
		memOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		dataOff, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		length, e := st.pop()
		if e != nil {
			return false, nil, e
		}
		// Strict bounds: reading past the return data faults, not zero-pads,
		// so a miscomputed size fails loudly.
		end := new(big.Int).Add(dataOff, length)
		if !end.IsUint64() || end.Uint64() > uint64(len(in.returnData)) {
			return false, nil, ErrReturnDataOutOfBounds
		}
		if e := in.chargeMem(memOff, length); e != nil {
			return false, nil, e
		}
		if e := in.useGas(3 * toWordSize(length.Uint64())); e != nil {
			return false, nil, e
		}
		in.mem.set(memOff.Uint64(), in.returnData[dataOff.Uint64():end.Uint64()])
		return false, nil, nil

	case LOG0, LOG0 + 1, LOG0 + 2, LOG0 + 3, LOG0 + 4:
		return false, nil, in.opLog(int(op - LOG0))

	case CREATE:
		return false, nil, in.opCreate(false)
	case CREATE2:
		return false, nil, in.opCreate(true)
	case CALL:
		return false, nil, in.opCall(callRegular)
	case DELEGATECALL:
		return false, nil, in.opCall(callDelegate)
	case STATICCALL:
		return false, nil, in.opCall(callStatic)

	case RETURN:
		data, e := in.readMem()
		return true, data, e

	case REVERT:
		data, e := in.readMem()
		if e != nil {
			return true, nil, e
		}
		return true, data, ErrReverted

	case INVALID:
		return false, nil, ErrInvalidOpcode

	default:
		return false, nil, ErrInvalidOpcode
	}
}

// binop applies a two-operand op (a on top, b below) and pushes the result.
func (in *interp) binop(op OpCode, a, b *big.Int) error {
	z := new(big.Int)
	switch op {
	case ADD:
		z.Add(a, b)
	case MUL:
		z.Mul(a, b)
	case SUB:
		z.Sub(a, b)
	case DIV:
		if b.Sign() == 0 {
			z.Set(big0) // EVM: division by zero is zero, not a fault
		} else {
			z.Div(a, b)
		}
	case SDIV:
		// Signed division truncated toward zero (Quo, not Div). Edge case
		// INT_MIN / -1 = 2^255 does not fit a signed word and wraps to INT_MIN,
		// which toUnsigned's mod 2^256 produces without a special case.
		sb := toSigned(b)
		if sb.Sign() == 0 {
			z.Set(big0)
		} else {
			z = toUnsigned(new(big.Int).Quo(toSigned(a), sb))
		}
	case MOD:
		if b.Sign() == 0 {
			z.Set(big0)
		} else {
			z.Mod(a, b)
		}
	case SMOD:
		// Signed remainder takes the sign of the dividend (Rem, truncated).
		sb := toSigned(b)
		if sb.Sign() == 0 {
			z.Set(big0)
		} else {
			z = toUnsigned(new(big.Int).Rem(toSigned(a), sb))
		}
	case SIGNEXTEND:
		// a = byte index of the sign bit (0 = LSB); b = value. Copy bit 8*a+7 of
		// b into every higher bit. a >= 31: sign byte is already the top, b
		// unchanged.
		if a.Cmp(big.NewInt(31)) > 0 {
			z.Set(b)
		} else {
			bitPos := uint(8*a.Uint64() + 7)
			mask := new(big.Int).Sub(new(big.Int).Lsh(big1, bitPos+1), big1) // low bits kept
			if b.Bit(int(bitPos)) == 1 {
				z.Or(b, new(big.Int).Xor(tt256m1, mask)) // set all higher bits
			} else {
				z.And(b, mask) // clear all higher bits
			}
		}
	case BYTE:
		// a = byte index from the MSB (0..31); b = value. Out-of-range yields 0.
		if a.Cmp(big.NewInt(32)) >= 0 {
			z.Set(big0)
		} else {
			shift := uint(8 * (31 - a.Uint64()))
			z.And(new(big.Int).Rsh(b, shift), big.NewInt(0xff))
		}
	case EXP:
		// a**b mod 2^256, extra gas per exponent byte so a giant exponent is not
		// cheap.
		if e := in.useGas(50 * uint64(byteLen(b))); e != nil {
			return e
		}
		z.Exp(a, b, tt256)
	case LT:
		z.SetInt64(boolToInt(a.Cmp(b) < 0))
	case GT:
		z.SetInt64(boolToInt(a.Cmp(b) > 0))
	case SLT:
		z.SetInt64(boolToInt(toSigned(a).Cmp(toSigned(b)) < 0))
	case SGT:
		z.SetInt64(boolToInt(toSigned(a).Cmp(toSigned(b)) > 0))
	case EQ:
		z.SetInt64(boolToInt(a.Cmp(b) == 0))
	case AND:
		z.And(a, b)
	case OR:
		z.Or(a, b)
	case XOR:
		z.Xor(a, b)
	case SHL:
		if a.Cmp(big.NewInt(256)) >= 0 {
			z.Set(big0)
		} else {
			z.Lsh(b, uint(a.Uint64()))
		}
	case SHR:
		if a.Cmp(big.NewInt(256)) >= 0 {
			z.Set(big0)
		} else {
			z.Rsh(b, uint(a.Uint64()))
		}
	case SAR:
		// Arithmetic (sign-propagating) right shift. Shift >= 256 collapses to
		// the sign: 0 if non-negative, -1 if negative. big.Int.Rsh floors toward
		// -inf, matching SAR.
		sb := toSigned(b)
		if a.Cmp(big.NewInt(256)) >= 0 {
			if sb.Sign() < 0 {
				z.Set(tt256m1) // -1 in two's complement
			} else {
				z.Set(big0)
			}
		} else {
			z = toUnsigned(new(big.Int).Rsh(sb, uint(a.Uint64())))
		}
	}
	return in.stack.push(wrap(z))
}

// tt255 = 2^255, the non-negative/negative boundary for a two's-complement
// 256-bit word (>= tt255 has the top bit set).
var tt255 = new(big.Int).Lsh(big.NewInt(1), 255)

// toSigned reinterprets an unsigned 256-bit word as a signed two's-complement
// integer: values with the high bit set become negative.
func toSigned(x *big.Int) *big.Int {
	if x.Cmp(tt255) < 0 {
		return new(big.Int).Set(x)
	}
	return new(big.Int).Sub(x, tt256)
}

// toUnsigned reduces a signed result into the [0, 2^256) two's-complement
// encoding. Mod (non-negative for a positive modulus) maps -1 to 2^256-1.
func toUnsigned(x *big.Int) *big.Int { return new(big.Int).Mod(x, tt256) }

// jump validates a destination and moves pc there.
func (in *interp) jump(dest *big.Int) error {
	if !dest.IsUint64() {
		return ErrInvalidJump
	}
	d := dest.Uint64()
	if d >= uint64(len(in.jumps)) || !in.jumps[d] {
		return ErrInvalidJump
	}
	in.pc = d
	in.jumped = true
	return nil
}

// chargeMem charges the gas to grow memory to cover [off, off+size) and grows
// it, refusing offsets that overflow uint64.
func (in *interp) chargeMem(off, size *big.Int) error {
	// A zero-length access touches no memory, whatever the offset. Without this,
	// a size-0 LOG/RETURN/CALL with a huge offset would resize(off) and try to
	// allocate `off` bytes for nothing (node-crashing panic, found by fuzzing).
	if size.Sign() == 0 {
		return nil
	}
	if !off.IsUint64() || !size.IsUint64() {
		return ErrOutOfGas // an offset this large can never be paid for
	}
	gas, ok := in.mem.expansionGas(off.Uint64(), size.Uint64())
	if !ok {
		return ErrOutOfGas
	}
	if err := in.useGas(gas); err != nil {
		return err
	}
	in.mem.resize(off.Uint64() + size.Uint64())
	return nil
}

// readMem pops (offset, size) and returns that memory slice, charging for
// expansion. Shared by RETURN and REVERT.
func (in *interp) readMem() ([]byte, error) {
	off, e := in.stack.pop()
	if e != nil {
		return nil, e
	}
	size, e := in.stack.pop()
	if e != nil {
		return nil, e
	}
	if e := in.chargeMem(off, size); e != nil {
		return nil, e
	}
	return in.mem.get(off.Uint64(), size.Uint64()), nil
}

// opLog implements LOG0..LOG4 (n = indexed-topic count). Pops the data window
// then n topics, charges base + per-topic + per-byte, and records the event
// against the running address. A log in a frame that reverts disappears with it
// (AddLog rides the storage snapshot stack).
func (in *interp) opLog(n int) error {
	if in.ctx.Static {
		return ErrStaticStateChange // an event is a state change; not in a read-only frame
	}
	off, err := in.stack.pop()
	if err != nil {
		return err
	}
	size, err := in.stack.pop()
	if err != nil {
		return err
	}
	topics := make([]common.Hash, n)
	for i := 0; i < n; i++ {
		t, err := in.stack.pop()
		if err != nil {
			return err
		}
		topics[i] = wordToHash(t)
	}
	if err := in.chargeMem(off, size); err != nil {
		return err
	}
	if err := in.useGas(LogGas + uint64(n)*LogTopicGas); err != nil {
		return err
	}
	sz := size.Uint64() // fits: chargeMem already refused an unpayable size
	if err := in.useGas(LogDataGas * sz); err != nil {
		return err
	}
	if in.ctx.State == nil {
		return ErrNoState
	}
	in.ctx.State.AddLog(&common.Log{
		Address: in.ctx.Address,
		Topics:  topics,
		Data:    append([]byte(nil), in.mem.get(off.Uint64(), sz)...),
	})
	return nil
}

// bigOrZero returns a copy of v, or a fresh zero if v is nil, so an unset
// optional big field does not panic.
func bigOrZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(v)
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func byteLen(x *big.Int) int { return (x.BitLen() + 7) / 8 }

// getData reads size bytes from data at offset, zero-padded past the end or for
// an out-of-range offset (EVM calldata reads never fault).
func getData(data []byte, offset *big.Int, size uint64) []byte {
	out := make([]byte, size)
	if size == 0 || !offset.IsUint64() {
		return out
	}
	off := offset.Uint64()
	if off >= uint64(len(data)) {
		return out
	}
	copy(out, data[off:min(off+size, uint64(len(data)))])
	return out
}
