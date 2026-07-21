package vm

import "encoding/binary"

// ripemd160 computes the 20-byte RIPEMD-160 digest of msg from scratch, so the
// default build has no external crypto dependency (x/crypto/ripemd160 is
// deprecated).
//
// RIPEMD-160 backs the Ethereum 0x03 precompile. Each 512-bit block runs
// through two parallel 80-step compression lines that are then combined; the
// constant tables below are the algorithm verbatim. A wrong table entry
// silently corrupts every digest, so TestRIPEMD160 pins known-answer vectors.
func ripemd160(msg []byte) [20]byte {
	h := [5]uint32{0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0}

	// Padding: 0x80, zeros to 56 mod 64, then the 64-bit little-endian bit
	// length. Little-endian throughout (unlike the big-endian SHA family).
	bitLen := uint64(len(msg)) * 8
	padded := append([]byte(nil), msg...)
	padded = append(padded, 0x80)
	for len(padded)%64 != 56 {
		padded = append(padded, 0)
	}
	var lb [8]byte
	binary.LittleEndian.PutUint64(lb[:], bitLen)
	padded = append(padded, lb[:]...)

	for off := 0; off < len(padded); off += 64 {
		var x [16]uint32
		for i := 0; i < 16; i++ {
			x[i] = binary.LittleEndian.Uint32(padded[off+i*4:])
		}
		h = ripemd160Block(h, x)
	}

	var out [20]byte
	for i := 0; i < 5; i++ {
		binary.LittleEndian.PutUint32(out[i*4:], h[i])
	}
	return out
}

func rotl32(x uint32, n uint) uint32 { return (x << n) | (x >> (32 - n)) }

// ripemdF is the round function; which boolean mix applies depends on the step.
func ripemdF(j int, x, y, z uint32) uint32 {
	switch {
	case j < 16:
		return x ^ y ^ z
	case j < 32:
		return (x & y) | (^x & z)
	case j < 48:
		return (x | ^y) ^ z
	case j < 64:
		return (x & z) | (y & ^z)
	default:
		return x ^ (y | ^z)
	}
}

// Added constants per round, for the left and right lines.
func ripemdKL(j int) uint32 {
	switch {
	case j < 16:
		return 0x00000000
	case j < 32:
		return 0x5A827999
	case j < 48:
		return 0x6ED9EBA1
	case j < 64:
		return 0x8F1BBCDC
	default:
		return 0xA953FD4E
	}
}

func ripemdKR(j int) uint32 {
	switch {
	case j < 16:
		return 0x50A28BE6
	case j < 32:
		return 0x5C4DD124
	case j < 48:
		return 0x6D703EF3
	case j < 64:
		return 0x7A6D76E9
	default:
		return 0x00000000
	}
}

// Message-word selection order and rotate amounts, left (…L) and right (…R).
var (
	ripemdRL = [80]int{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		7, 4, 13, 1, 10, 6, 15, 3, 12, 0, 9, 5, 2, 14, 11, 8,
		3, 10, 14, 4, 9, 15, 8, 1, 2, 7, 0, 6, 13, 11, 5, 12,
		1, 9, 11, 10, 0, 8, 12, 4, 13, 3, 7, 15, 14, 5, 6, 2,
		4, 0, 5, 9, 7, 12, 2, 10, 14, 1, 3, 8, 11, 6, 15, 13,
	}
	ripemdRR = [80]int{
		5, 14, 7, 0, 9, 2, 11, 4, 13, 6, 15, 8, 1, 10, 3, 12,
		6, 11, 3, 7, 0, 13, 5, 10, 14, 15, 8, 12, 4, 9, 1, 2,
		15, 5, 1, 3, 7, 14, 6, 9, 11, 8, 12, 2, 10, 0, 4, 13,
		8, 6, 4, 1, 3, 11, 15, 0, 5, 12, 2, 13, 9, 7, 10, 14,
		12, 15, 10, 4, 1, 5, 8, 7, 6, 2, 13, 14, 0, 3, 9, 11,
	}
	ripemdSL = [80]uint{
		11, 14, 15, 12, 5, 8, 7, 9, 11, 13, 14, 15, 6, 7, 9, 8,
		7, 6, 8, 13, 11, 9, 7, 15, 7, 12, 15, 9, 11, 7, 13, 12,
		11, 13, 6, 7, 14, 9, 13, 15, 14, 8, 13, 6, 5, 12, 7, 5,
		11, 12, 14, 15, 14, 15, 9, 8, 9, 14, 5, 6, 8, 6, 5, 12,
		9, 15, 5, 11, 6, 8, 13, 12, 5, 12, 13, 14, 11, 8, 5, 6,
	}
	ripemdSR = [80]uint{
		8, 9, 9, 11, 13, 15, 15, 5, 7, 7, 8, 11, 14, 14, 12, 6,
		9, 13, 15, 7, 12, 8, 9, 11, 7, 7, 12, 7, 6, 15, 13, 11,
		9, 7, 15, 11, 8, 6, 6, 14, 12, 13, 5, 14, 13, 13, 7, 5,
		15, 5, 8, 11, 14, 14, 6, 14, 6, 9, 12, 9, 12, 5, 15, 8,
		8, 5, 12, 9, 12, 5, 14, 6, 8, 13, 6, 5, 15, 13, 11, 11,
	}
)

// ripemd160Block runs one 512-bit block through both lines and folds the two
// results back into the running state h.
func ripemd160Block(h [5]uint32, x [16]uint32) [5]uint32 {
	al, bl, cl, dl, el := h[0], h[1], h[2], h[3], h[4]
	ar, br, cr, dr, er := h[0], h[1], h[2], h[3], h[4]

	for j := 0; j < 80; j++ {
		t := rotl32(al+ripemdF(j, bl, cl, dl)+x[ripemdRL[j]]+ripemdKL(j), ripemdSL[j]) + el
		al, el, dl, cl, bl = el, dl, rotl32(cl, 10), bl, t

		t = rotl32(ar+ripemdF(79-j, br, cr, dr)+x[ripemdRR[j]]+ripemdKR(j), ripemdSR[j]) + er
		ar, er, dr, cr, br = er, dr, rotl32(cr, 10), br, t
	}

	// The cross-over combine: each new word mixes one register from each line.
	return [5]uint32{
		h[1] + cl + dr,
		h[2] + dl + er,
		h[3] + el + ar,
		h[4] + al + br,
		h[0] + bl + cr,
	}
}
