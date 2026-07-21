package vm

import (
	"crypto/sha256"

	"lxs/common"
	"lxs/crypto"
)

// Precompiles are contracts at fixed low addresses (0x01..0x05) that run native
// Go instead of bytecode, for primitives too expensive as EVM opcodes (EC
// recovery, SHA-256, big-integer modexp). A CALL to one is intercepted in opCall
// and routed here.
//
// Gas is a flat base plus a per-32-byte-word surcharge on the input (Ethereum),
// so a caller cannot hash a megabyte for the base fee alone.
type precompile struct {
	name string
	gas  func(input []byte) uint64
	// run returns the output, or an error to FAIL the call. Most precompiles
	// cannot fail on well-gassed input (a bad ecrecover returns the zero address,
	// success), but the bn256 group ops reject a malformed/off-curve point — that
	// must fail the CALL (caller sees 0), never return empty+success, or a contract
	// reads a forged "valid" result.
	run func(input []byte) ([]byte, error)
}

// precompileFor returns the precompile at addr, or nil for an ordinary account.
// Precompiles are 0x00…01 through 0x00…04, so the high 19 bytes must be zero.
func precompileFor(addr common.Address) *precompile {
	for i := 0; i < len(addr)-1; i++ {
		if addr[i] != 0 {
			return nil
		}
	}
	switch addr[len(addr)-1] {
	case 1:
		return &ecrecoverPrecompile
	case 2:
		return &sha256Precompile
	case 3:
		return &ripemd160Precompile
	case 4:
		return &identityPrecompile
	case 5:
		return &modexpPrecompile
	case 6:
		return &bn256AddPrecompile
	case 7:
		return &bn256ScalarMulPrecompile
	case 8:
		return &bn256PairingPrecompile
	}
	return nil
}

// runPrecompile charges the precompile's gas from the call budget and runs it.
// Insufficient gas is a hard out-of-gas, like any contract; the caller sees a
// failed call, handled in opCall.
func runPrecompile(p *precompile, input []byte, gas uint64) Result {
	cost := p.gas(input)
	if gas < cost {
		return Result{GasLeft: 0, Err: ErrOutOfGas}
	}
	ret, err := p.run(input)
	if err != nil {
		// A precompile fault (a malformed EC point) fails the CALL and burns all
		// the forwarded gas, exactly like an EVM hard fault — not a refundable
		// revert, and never a silent empty success.
		return Result{GasLeft: 0, Err: err}
	}
	return Result{Ret: ret, GasLeft: gas - cost}
}

// linearWordGas builds the base + perWord*ceil(len/32) pricing shared by the
// hashing and identity precompiles.
func linearWordGas(base, perWord uint64) func([]byte) uint64 {
	return func(input []byte) uint64 {
		return base + perWord*toWordSize(uint64(len(input)))
	}
}

// 0x04 IDENTITY: returns its input unchanged (used to copy calldata/returndata
// regions). The copy is defensive so the caller cannot alias VM memory.
var identityPrecompile = precompile{
	name: "identity",
	gas:  linearWordGas(15, 3),
	run:  func(in []byte) ([]byte, error) { return append([]byte(nil), in...), nil },
}

// 0x02 SHA256: stdlib SHA-256, no dependency.
var sha256Precompile = precompile{
	name: "sha256",
	gas:  linearWordGas(60, 12),
	run: func(in []byte) ([]byte, error) {
		h := sha256.Sum256(in)
		return h[:], nil
	},
}

// 0x03 RIPEMD160: 20-byte digest, left-padded into a 32-byte word (high 12
// bytes zero). Implemented from scratch (ripemd160.go) to avoid the deprecated
// x/crypto/ripemd160 dependency.
var ripemd160Precompile = precompile{
	name: "ripemd160",
	gas:  linearWordGas(600, 120),
	run: func(in []byte) ([]byte, error) {
		d := ripemd160(in)
		out := make([]byte, 32)
		copy(out[12:], d[:])
		return out, nil
	},
}

// 0x01 ECRECOVER: recover the signer address from (hash, v, r, s).
var ecrecoverPrecompile = precompile{
	name: "ecrecover",
	gas:  func([]byte) uint64 { return 3000 },
	run:  ecrecoverRun,
}

// ecrecoverRun parses the 128-byte input and returns the left-padded signer
// address, or empty on any invalid input.
//
// Empty (not revert) on bad input is the EVM contract: the CALL succeeds and
// the caller checks for the zero address. Deliberate divergence: high-s
// signatures are rejected (crypto.Recover enforces low-s), since low-s is a
// chain-wide invariant.
func ecrecoverRun(input []byte) ([]byte, error) {
	// Right-zero-pad short input, exactly as a calldata read would.
	in := make([]byte, 128)
	copy(in, input)

	// v sits in the last byte of the second word; the other 31 bytes of that
	// word must be zero, and v itself must be the raw 27/28 (no EIP-155 form).
	for i := 32; i < 63; i++ {
		if in[i] != 0 {
			return nil, nil
		}
	}
	v := in[63]
	if v != 27 && v != 28 {
		return nil, nil
	}

	var digest common.Hash
	copy(digest[:], in[0:32])

	// crypto.Recover wants a compact [v | R(32) | S(32)] signature.
	sig := make([]byte, 65)
	sig[0] = v
	copy(sig[1:33], in[64:96])
	copy(sig[33:65], in[96:128])

	addr, err := crypto.RecoverAddress(digest, sig)
	if err != nil {
		return nil, nil // bad signature -> zero address, a SUCCESSFUL call (EVM contract)
	}
	out := make([]byte, 32)
	copy(out[12:], addr[:])
	return out, nil
}
