package common

import (
	"encoding/binary"
	"math/big"
)

// Canonical encoding. Two nodes must produce byte-identical encodings for the
// same object or their hashes diverge and consensus is impossible.
//
// Rules:
//   - integers are big-endian, fixed width
//   - big.Int is length-prefixed minimal big-endian (no leading zeros)
//   - byte slices are length-prefixed
//   - optional fields get a 1-byte presence flag
//
// Never hash encoding/json or encoding/gob output: map and field ordering are
// not guaranteed there.

type Encoder struct{ buf []byte }

func NewEncoder() *Encoder { return &Encoder{buf: make([]byte, 0, 256)} }

func (e *Encoder) Uint64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

func (e *Encoder) Int64(v int64) { e.Uint64(uint64(v)) }

func (e *Encoder) Bytes(b []byte) {
	e.Uint64(uint64(len(b)))
	e.buf = append(e.buf, b...)
}

// raw writes fixed-width data with no length prefix (for Hash/Address).
func (e *Encoder) Raw(b []byte) { e.buf = append(e.buf, b...) }

func (e *Encoder) BigInt(v *big.Int) {
	if v == nil {
		e.Bytes(nil)
		return
	}
	// big.Int.Bytes() is minimal big-endian, so the encoding is canonical.
	e.Bytes(v.Bytes())
}

func (e *Encoder) OptionalAddress(a *Address) {
	if a == nil {
		e.buf = append(e.buf, 0)
		return
	}
	e.buf = append(e.buf, 1)
	e.Raw(a[:])
}

func (e *Encoder) Done() []byte { return e.buf }
