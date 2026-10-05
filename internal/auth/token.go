// Package auth creates and checks service tokens (SRS — Core UC-105, ADR-7).
// Format: cas_<key_id>_<secret>. key_id is 6 random bytes as 12 lower-case hexadecimal characters;
// secret is 32 random bytes as 64 lower-case hexadecimal characters. Only the SHA-256 hash of the
// 32 secret bytes is stored.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
)

const (
	prefix      = "cas_"
	keyIDBytes  = 6
	secretBytes = 32
	keyIDLen    = 2 * keyIDBytes
	secretLen   = 2 * secretBytes
	tokenLen    = len(prefix) + keyIDLen + 1 + secretLen
)

// ErrMalformed is the one error of Parse. It does not say which part of the token is wrong.
var ErrMalformed = errors.New("auth: malformed service token")

// New returns a new token, its key_id and the hash to store.
func New() (token, keyID string, hash []byte, err error) {
	raw := make([]byte, keyIDBytes+secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", nil, err
	}
	keyID = hex.EncodeToString(raw[:keyIDBytes])
	secret := raw[keyIDBytes:]
	return prefix + keyID + "_" + hex.EncodeToString(secret), keyID, Hash(secret), nil
}

// Parse splits a token into its key_id and the secret bytes.
func Parse(token string) (keyID string, secret []byte, err error) {
	if len(token) != tokenLen || token[:len(prefix)] != prefix || token[len(prefix)+keyIDLen] != '_' {
		return "", nil, ErrMalformed
	}
	keyID = token[len(prefix) : len(prefix)+keyIDLen]
	secretHex := token[len(prefix)+keyIDLen+1:]
	if !isLowerHex(keyID) || !isLowerHex(secretHex) {
		return "", nil, ErrMalformed
	}
	secret, err = hex.DecodeString(secretHex)
	if err != nil {
		return "", nil, ErrMalformed
	}
	return keyID, secret, nil
}

// Hash returns the SHA-256 hash of the secret bytes.
func Hash(secret []byte) []byte {
	sum := sha256.Sum256(secret)
	return sum[:]
}

// Verify reports whether secret matches hash. The comparison takes constant time.
func Verify(secret, hash []byte) bool {
	return subtle.ConstantTimeCompare(Hash(secret), hash) == 1
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
