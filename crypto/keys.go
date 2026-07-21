package crypto

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"

	"lxs/common"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// SignatureLength is a compact recoverable signature: [recovery_id | R | S].
const SignatureLength = 65

type PrivateKey struct{ inner *secp256k1.PrivateKey }
type PublicKey struct{ inner *secp256k1.PublicKey }

func GenerateKey() (*PrivateKey, error) {
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return &PrivateKey{inner: k}, nil
}

func PrivateKeyFromHex(s string) (*PrivateKey, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, errors.New("crypto: private key must be 32 bytes")
	}
	return &PrivateKey{inner: secp256k1.PrivKeyFromBytes(b)}, nil
}

func (k *PrivateKey) Hex() string        { return "0x" + hex.EncodeToString(k.inner.Serialize()) }
func (k *PrivateKey) Public() *PublicKey { return &PublicKey{inner: k.inner.PubKey()} }
func (k *PrivateKey) Address() common.Address {
	return k.Public().Address()
}

// Address derives the account address: keccak256(uncompressed_pubkey[1:])[12:],
// stripping the leading 0x04 tag. Ethereum's scheme, so existing wallets and
// key tooling work unmodified.
func (p *PublicKey) Address() common.Address {
	raw := p.inner.SerializeUncompressed() // 65 bytes: 0x04 || X || Y
	h := common.Keccak256(raw[1:])
	var a common.Address
	copy(a[:], h[12:])
	return a
}

func (p *PublicKey) Bytes() []byte { return p.inner.SerializeCompressed() }

// Sign produces a compact recoverable signature over a 32-byte digest.
// Recoverable is why a transaction carries no from field: the sender is derived
// from the signature and cannot be claimed.
func Sign(digest common.Hash, key *PrivateKey) ([]byte, error) {
	sig := ecdsa.SignCompact(key.inner, digest[:], false)
	if len(sig) != SignatureLength {
		return nil, errors.New("crypto: unexpected signature length")
	}
	return sig, nil
}

// halfOrder is N/2 for the secp256k1 curve order N.
//
// For every valid signature (r, s) there is a second (r, N-s) over the same
// message from the same key: signature malleability. Since a tx hash covers its
// signature, flipping s yields a different hash with the same sender, nonce, and
// effect. The wallet waits on the hash it sent while the producer mines the
// other; the receipt lookup returns null forever. EIP-2 closed this by rejecting
// high-s. Sign already produces low-s, but an attacker need not use it, so only
// verification can enforce the rule.
var halfOrder = new(big.Int).Rsh(secp256k1.S256().N, 1)

// ErrHighS rejects the non-canonical half of every signature pair.
var ErrHighS = errors.New("crypto: non-canonical high-s signature")

// checkLowS enforces s <= N/2 over a compact [v | R(32) | S(32)] signature.
func checkLowS(sig []byte) error {
	if len(sig) != SignatureLength {
		return errors.New("crypto: bad signature length")
	}
	s := new(big.Int).SetBytes(sig[33:65])
	if s.Cmp(halfOrder) > 0 {
		return ErrHighS
	}
	return nil
}

// Recover returns the public key that produced sig over digest.
func Recover(digest common.Hash, sig []byte) (*PublicKey, error) {
	// Low-s first: a malleated signature is not valid here, whatever the curve says.
	if err := checkLowS(sig); err != nil {
		return nil, err
	}
	if len(sig) != SignatureLength {
		return nil, errors.New("crypto: bad signature length")
	}
	pub, _, err := ecdsa.RecoverCompact(sig, digest[:])
	if err != nil {
		return nil, err
	}
	return &PublicKey{inner: pub}, nil
}

// RecoverAddress is the hot path: signature + digest -> sender.
func RecoverAddress(digest common.Hash, sig []byte) (common.Address, error) {
	pub, err := Recover(digest, sig)
	if err != nil {
		return common.ZeroAddress, err
	}
	return pub.Address(), nil
}
