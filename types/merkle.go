package types

import "lxs/common"

// Domain separation tags. Without them a Merkle tree has a second-preimage
// weakness: a 64-byte internal node can be reinterpreted as a leaf, proving
// membership of data never in the tree. Distinct tags make leaf and node
// preimages disjoint (RFC 6962).
var (
	leafTag = []byte{0x00}
	nodeTag = []byte{0x01}
)

func hashLeaf(data []byte) common.Hash {
	return common.Keccak256(leafTag, data)
}

func hashNode(l, r common.Hash) common.Hash {
	return common.Keccak256(nodeTag, l.Bytes(), r.Bytes())
}

// MerkleRoot computes the root over an ordered list of leaves.
// An empty list hashes to the zero hash.
func MerkleRoot(leaves [][]byte) common.Hash {
	if len(leaves) == 0 {
		return common.ZeroHash
	}
	layer := make([]common.Hash, len(leaves))
	for i, l := range leaves {
		layer[i] = hashLeaf(l)
	}
	for len(layer) > 1 {
		next := make([]common.Hash, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			if i+1 == len(layer) {
				// Odd node is promoted, not duplicated. Bitcoin duplicates
				// the last hash, which created CVE-2012-2459: two different
				// tx lists producing the same root. Promotion cannot.
				next = append(next, layer[i])
				continue
			}
			next = append(next, hashNode(layer[i], layer[i+1]))
		}
		layer = next
	}
	return layer[0]
}

// TxRoot commits to the transaction list of a block.
func TxRoot(txs []*Transaction) common.Hash {
	leaves := make([][]byte, len(txs))
	for i, tx := range txs {
		leaves[i] = tx.Hash().Bytes()
	}
	return MerkleRoot(leaves)
}
