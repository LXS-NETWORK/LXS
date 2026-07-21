package types

import (
	"testing"

	"lxs/common"
)

func TestMerkleEmpty(t *testing.T) {
	if MerkleRoot(nil) != common.ZeroHash {
		t.Fatal("empty tree should be the zero hash")
	}
}

func TestMerkleSingle(t *testing.T) {
	r := MerkleRoot([][]byte{[]byte("a")})
	if r == common.ZeroHash {
		t.Fatal("single-leaf root should not be zero")
	}
}

func TestMerkleOrderMatters(t *testing.T) {
	a := MerkleRoot([][]byte{[]byte("a"), []byte("b")})
	b := MerkleRoot([][]byte{[]byte("b"), []byte("a")})
	if a == b {
		t.Fatal("merkle root must commit to ordering")
	}
}

// CVE-2012-2459 regression. Bitcoin duplicates the final hash on an odd layer,
// so [a,b,c] and [a,b,c,c] produce the same root: an attacker could mutate a
// block to still match the header. Promotion avoids this; this checks it was not
// reintroduced.
func TestMerkleNoDuplicationCollision(t *testing.T) {
	three := MerkleRoot([][]byte{[]byte("a"), []byte("b"), []byte("c")})
	four := MerkleRoot([][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("c")})
	if three == four {
		t.Fatal("CVE-2012-2459: [a,b,c] and [a,b,c,c] collide")
	}
}

// Second-preimage resistance: an internal node's preimage must never be a valid
// leaf preimage. The domain separation tags provide this.
func TestMerkleLeafNodeDomainSeparation(t *testing.T) {
	l := hashLeaf([]byte("x"))
	// Feed the concatenation of two leaves in as a single leaf. Without
	// tags this could equal the parent node hash.
	pair := append(l.Bytes(), l.Bytes()...)
	if hashLeaf(pair) == hashNode(l, l) {
		t.Fatal("leaf and internal node hashes are not domain separated")
	}
}

func TestMerkleDeterministic(t *testing.T) {
	leaves := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")}
	first := MerkleRoot(leaves)
	for i := 0; i < 100; i++ {
		if MerkleRoot(leaves) != first {
			t.Fatal("merkle root is not deterministic")
		}
	}
}
