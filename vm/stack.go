package vm

import (
	"errors"
	"math/big"
)

// maxStack is the EVM hard depth limit; bounds stack growth. Solidity's
// "stack too deep" is this limit.
const maxStack = 1024

var (
	ErrStackUnderflow = errors.New("vm: stack underflow")
	ErrStackOverflow  = errors.New("vm: stack overflow")
)

// Stack is the EVM operand stack: 256-bit words, LIFO.
//
// Words are *big.Int reduced mod 2^256. EVM arithmetic wraps at 256 bits, so
// every overflowing operation calls wrap(); an unreduced 257th bit would
// diverge from mainnet on the first overflow.
type Stack struct {
	data []*big.Int
}

func NewStack() *Stack { return &Stack{data: make([]*big.Int, 0, 16)} }

// push adds a word, refusing to exceed the depth limit.
func (s *Stack) push(v *big.Int) error {
	if len(s.data) >= maxStack {
		return ErrStackOverflow
	}
	s.data = append(s.data, wrap(v))
	return nil
}

// pop removes and returns the top word.
func (s *Stack) pop() (*big.Int, error) {
	n := len(s.data)
	if n == 0 {
		return nil, ErrStackUnderflow
	}
	v := s.data[n-1]
	s.data = s.data[:n-1]
	return v, nil
}

// peek returns the word n slots from the top without removing it (0 = top).
func (s *Stack) peek(n int) (*big.Int, error) {
	if n >= len(s.data) {
		return nil, ErrStackUnderflow
	}
	return s.data[len(s.data)-1-n], nil
}

// dup pushes a copy of the word n slots down (DUPn: n from 1).
func (s *Stack) dup(n int) error {
	v, err := s.peek(n - 1)
	if err != nil {
		return err
	}
	return s.push(new(big.Int).Set(v))
}

// swap exchanges the top with the word n slots down (SWAPn: n from 1).
func (s *Stack) swap(n int) error {
	if n >= len(s.data) {
		return ErrStackUnderflow
	}
	top := len(s.data) - 1
	s.data[top], s.data[top-n] = s.data[top-n], s.data[top]
	return nil
}

// tt256m1 is 2^256 - 1, the mask that reduces a word to 256 bits.
var tt256m1 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// wrap reduces v to the low 256 bits in place. EVM modular arithmetic: results
// live in [0, 2^256).
func wrap(v *big.Int) *big.Int { return v.And(v, tt256m1) }
