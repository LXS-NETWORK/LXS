package vm

import (
	"errors"
	"math/big"

	"github.com/clearmatics/bn256"
)

// The alt_bn128 (bn256) pairing precompiles, EIP-196 (0x06 add, 0x07 scalar mul)
// and EIP-197 (0x08 pairing check), priced at the EIP-1108 Istanbul schedule.
//
// These back on-chain zk-SNARK verification. Before they existed here a CALL to
// 0x06..0x08 hit no precompile and behaved as an empty account: the call
// "succeeded" and returned nothing, so a verifier contract read a zero result as a
// valid proof — a silent forgery. They now compute the real group operation and
// FAIL the call on a malformed point.
//
// Two properties are consensus-critical and are the reason this uses
// clearmatics/bn256 (go-ethereum's own subgroup-checked cloudflare fork, as a
// standalone module) rather than golang.org/x/crypto/bn256:
//   - Ethereum point encoding: G1 is 64 bytes (x‖y), G2 is 128 bytes
//     (x.im‖x.re‖y.im‖y.re), infinity is all-zeros — no leading tag byte.
//   - The G2 subgroup-membership check (Order·P == ∞) inside Unmarshal. Without it
//     an attacker submits an off-subgroup twist point and forges a pairing that
//     passes, breaking every SNARK verifier on the chain (the historical bn256
//     twist attack). The plain x/crypto library performs only an on-curve check.

const (
	bn256AddGas       = 150   // EIP-1108 Bn256AddGasIstanbul
	bn256ScalarMulGas = 6000  // EIP-1108 Bn256ScalarMulGasIstanbul
	bn256PairingBase  = 45000 // EIP-1108 Bn256PairingBaseGasIstanbul
	bn256PairingPer   = 34000 // EIP-1108 Bn256PairingPerPointGasIstanbul
	bn256PairSize     = 192   // 64-byte G1 + 128-byte G2 per pairing term
)

var errBadPairingInput = errors.New("bn256: pairing input not a multiple of 192")

// newG1 / newG2 parse a point in Ethereum encoding. Unmarshal rejects an
// off-curve point (and, for G2, an off-subgroup one), which fails the call.
func newG1(blob []byte) (*bn256.G1, error) {
	p := new(bn256.G1)
	if _, err := p.Unmarshal(blob); err != nil {
		return nil, err
	}
	return p, nil
}

func newG2(blob []byte) (*bn256.G2, error) {
	p := new(bn256.G2)
	if _, err := p.Unmarshal(blob); err != nil {
		return nil, err
	}
	return p, nil
}

// 0x06 ECADD: P + Q on G1.
var bn256AddPrecompile = precompile{
	name: "bn256Add",
	gas:  func([]byte) uint64 { return bn256AddGas },
	run: func(input []byte) ([]byte, error) {
		x, err := newG1(getData(input, big.NewInt(0), 64))
		if err != nil {
			return nil, err
		}
		y, err := newG1(getData(input, big.NewInt(64), 64))
		if err != nil {
			return nil, err
		}
		return new(bn256.G1).Add(x, y).Marshal(), nil
	},
}

// 0x07 ECMUL: scalar·P on G1. The scalar is a full 256-bit word, unreduced —
// ScalarMult handles it modulo the group order.
var bn256ScalarMulPrecompile = precompile{
	name: "bn256ScalarMul",
	gas:  func([]byte) uint64 { return bn256ScalarMulGas },
	run: func(input []byte) ([]byte, error) {
		p, err := newG1(getData(input, big.NewInt(0), 64))
		if err != nil {
			return nil, err
		}
		k := new(big.Int).SetBytes(getData(input, big.NewInt(64), 32))
		return new(bn256.G1).ScalarMult(p, k).Marshal(), nil
	},
}

// 0x08 PAIRING: returns 1 iff ∏ e(Gᵢ, Hᵢ) == 1. Input is k concatenated
// (G1,G2) pairs; empty input is the empty product, which is 1 (true). A length not
// a multiple of 192 fails the call (EIP-197), so a truncated final pair can never
// be silently zero-padded into a different check.
var bn256PairingPrecompile = precompile{
	name: "bn256Pairing",
	gas: func(input []byte) uint64 {
		return bn256PairingBase + uint64(len(input)/bn256PairSize)*bn256PairingPer
	},
	run: func(input []byte) ([]byte, error) {
		if len(input)%bn256PairSize != 0 {
			return nil, errBadPairingInput
		}
		var (
			cs []*bn256.G1
			ts []*bn256.G2
		)
		for i := 0; i < len(input); i += bn256PairSize {
			c, err := newG1(input[i : i+64])
			if err != nil {
				return nil, err
			}
			t, err := newG2(input[i+64 : i+bn256PairSize])
			if err != nil {
				return nil, err
			}
			cs = append(cs, c)
			ts = append(ts, t)
		}
		out := make([]byte, 32)
		if bn256.PairingCheck(cs, ts) {
			out[31] = 1
		}
		return out, nil
	},
}
