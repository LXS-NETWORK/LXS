package vm

import (
	"math/big"
)

// 0x05 MODEXP (EIP-198): arbitrary-precision (base**exp) % mod. The one precompile
// whose gas AND output size depend on the input, so both are derived from a parsed
// header rather than a flat table.
//
// Input layout, each length a 32-byte big-endian word:
//
//	[ baseLen | expLen | modLen | base | exp | mod ]
//
// Shorter input is zero-extended (a calldata read past the end reads zeros), so a
// truncated call is well-defined, never a panic.
//
// Gas uses the Istanbul (EIP-198) formula, NOT the cheaper EIP-2565 (Berlin): this
// chain targets Istanbul, and charging Berlin prices would let a contract buy more
// modexp work than every other node prices it at — a consensus split, not just a
// mispricing. GQUADDIVISOR is 20 here (EIP-2565 lowered it to 3).
var modexpPrecompile = precompile{
	name: "modexp",
	gas:  modexpGas,
	run:  modexpRun,
}

const modexpQuadDivisor = 20 // EIP-198 GQUADDIVISOR

// modexpHeader reads the three length words. Lengths are returned as *big.Int
// because an attacker can set modLen = 2**256-1: kept as big.Int the gas math
// overflows to an astronomical cost that simply out-of-gases, whereas a uint64
// truncation would wrap to a small length and under-charge. getData (from exec.go)
// zero-pads a read past the end, so a truncated header is well-defined.
func modexpHeader(input []byte) (baseLen, expLen, modLen *big.Int) {
	return new(big.Int).SetBytes(getData(input, big.NewInt(0), 32)),
		new(big.Int).SetBytes(getData(input, big.NewInt(32), 32)),
		new(big.Int).SetBytes(getData(input, big.NewInt(64), 32))
}

func modexpGas(input []byte) uint64 {
	baseLen, expLen, modLen := modexpHeader(input)

	// adjusted exponent length: 8*(expLen-32) for the whole-byte part beyond the
	// first word, plus the bit index of the highest set bit in the leading 32
	// bytes of the exponent. That head starts at offset 96+baseLen.
	expOff := new(big.Int).Add(big.NewInt(96), baseLen)
	var adjExpLen *big.Int
	if expLen.Cmp(big.NewInt(32)) <= 0 {
		// exponent fits in its declared (<=32) length: read it and take bitlen-1.
		expHead := new(big.Int).SetBytes(getData(input, expOff, expLen.Uint64()))
		adjExpLen = adjustedFromHead(expHead, big.NewInt(0))
	} else {
		expHead := new(big.Int).SetBytes(getData(input, expOff, 32))
		tail := new(big.Int).Sub(expLen, big.NewInt(32))
		adjExpLen = adjustedFromHead(expHead, new(big.Int).Mul(tail, big.NewInt(8)))
	}
	if adjExpLen.Sign() < 1 {
		adjExpLen = big.NewInt(1) // max(adjExpLen, 1)
	}

	// mult_complexity(max(baseLen, modLen)).
	maxLen := baseLen
	if modLen.Cmp(maxLen) > 0 {
		maxLen = modLen
	}
	cost := new(big.Int).Mul(multComplexity(maxLen), adjExpLen)
	cost.Div(cost, big.NewInt(modexpQuadDivisor))

	if !cost.IsUint64() {
		return ^uint64(0) // astronomically large -> guaranteed out-of-gas
	}
	return cost.Uint64()
}

// adjustedFromHead computes the exponent's iteration count: base (the whole-byte
// tail contribution, already ×8) plus the 0-indexed position of the highest set bit
// in the head word (bitlen-1), or just base when the head is zero.
func adjustedFromHead(head, base *big.Int) *big.Int {
	if head.Sign() == 0 {
		return new(big.Int).Set(base)
	}
	return new(big.Int).Add(base, big.NewInt(int64(head.BitLen()-1)))
}

// multComplexity is the EIP-198 piecewise cost of a big-integer multiplication of
// x-byte operands.
func multComplexity(x *big.Int) *big.Int {
	x2 := new(big.Int).Mul(x, x)
	switch {
	case x.Cmp(big.NewInt(64)) <= 0:
		return x2
	case x.Cmp(big.NewInt(1024)) <= 0:
		// x^2/4 + 96x - 3072
		r := new(big.Int).Div(x2, big.NewInt(4))
		r.Add(r, new(big.Int).Mul(x, big.NewInt(96)))
		return r.Sub(r, big.NewInt(3072))
	default:
		// x^2/16 + 480x - 199680
		r := new(big.Int).Div(x2, big.NewInt(16))
		r.Add(r, new(big.Int).Mul(x, big.NewInt(480)))
		return r.Sub(r, big.NewInt(199680))
	}
}

// modexpRun computes the result. It only runs once modexpGas has been paid, so the
// declared lengths are already small enough to afford — no separate size guard is
// needed to avoid an allocation bomb.
func modexpRun(input []byte) ([]byte, error) {
	baseLenB, expLenB, modLenB := modexpHeader(input)
	baseLen := baseLenB.Uint64()
	expLen := expLenB.Uint64()
	modLen := modLenB.Uint64()

	if modLen == 0 {
		return []byte{}, nil // no modulus -> empty result (EIP-198)
	}

	baseOff := big.NewInt(96)
	expOff := new(big.Int).Add(baseOff, baseLenB)
	modOff := new(big.Int).Add(expOff, expLenB)
	base := new(big.Int).SetBytes(getData(input, baseOff, baseLen))
	exp := new(big.Int).SetBytes(getData(input, expOff, expLen))
	mod := new(big.Int).SetBytes(getData(input, modOff, modLen))

	out := make([]byte, modLen)
	if mod.Sign() == 0 {
		return out, nil // x mod 0 is defined as 0 here, output the zero-filled word
	}
	res := new(big.Int).Exp(base, exp, mod)
	// Left-pad (big-endian) into exactly modLen bytes.
	res.FillBytes(out)
	return out, nil
}
