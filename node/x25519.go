package node

import (
	"crypto/ed25519"
	"crypto/sha512"
	"fmt"

	"filippo.io/edwards25519"
)

// x25519FromEd25519 derives an X25519 private key from an Ed25519 identity.
//
// A node already holds one identity key for enrollment. Deriving the encryption
// key from it means an operator manages one key per node rather than two that
// can drift out of sync, and the sealed-secret path automatically follows node
// identity.
//
// The derivation is the standard one: an Ed25519 private key's scalar is the
// clamped lower half of SHA-512 over its seed, and that scalar is directly
// usable as an X25519 scalar.
func x25519FromEd25519(private ed25519.PrivateKey) [32]byte {
	digest := sha512.Sum512(private.Seed())
	var scalar [32]byte
	copy(scalar[:], digest[:32])
	// Clamping is what makes the scalar valid for X25519.
	scalar[0] &= 248
	scalar[31] &= 127
	scalar[31] |= 64
	return scalar
}

// x25519FromEd25519Public converts an Ed25519 public key to its X25519
// counterpart by mapping the Edwards point to Montgomery form.
func x25519FromEd25519Public(public ed25519.PublicKey) ([32]byte, error) {
	var converted [32]byte
	point, err := new(edwards25519.Point).SetBytes(public)
	if err != nil {
		return converted, fmt.Errorf("node public key is not a valid Ed25519 point: %w", err)
	}
	copy(converted[:], point.BytesMontgomery())
	return converted, nil
}
