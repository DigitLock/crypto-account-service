// Package vault encrypts exchange secrets at rest (ADR-4, SRS — Core §3.2 Security). Standard library only.
//
// Block layout, format 1:
//
//	byte 0       format, 1
//	12 bytes     nonce of the data key
//	48 bytes     data key (32 random bytes) sealed with the master key
//	12 bytes     nonce of the secret
//	rest         secret sealed with the data key
//
// Both seals are AES-256-GCM. Nonces and the data key are new for every Encrypt. The additional data of
// both seals is the 16 bytes of the connection ID, then the kek version as 2 bytes big-endian: a block
// cannot be moved to another row or read under another master key version.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	formatV1  = 1
	keySize   = 32
	nonceSize = 12
	tagSize   = 16

	wrappedKeySize = keySize + tagSize
	minBlockSize   = 1 + nonceSize + wrappedKeySize + nonceSize + tagSize
)

// ErrDecrypt is the one error of Decrypt. It says nothing about the cause.
var ErrDecrypt = errors.New("vault: cannot decrypt the secret")

// Vault encrypts and decrypts the secret of one connection.
type Vault interface {
	Encrypt(connectionID [16]byte, plaintext []byte) (block []byte, kekVersion int16, err error)
	Decrypt(connectionID [16]byte, block []byte, kekVersion int16) ([]byte, error)
}

// Envelope is the Vault on the master key of the environment (CAS_MASTER_KEY, CAS_MASTER_KEY_VERSION).
type Envelope struct {
	master  cipher.AEAD
	version int16
}

// New returns the vault on a 32-byte master key and its version, 1 to 32767.
func New(masterKey []byte, version int) (*Envelope, error) {
	if len(masterKey) != keySize {
		return nil, fmt.Errorf("vault: the master key must be %d bytes", keySize)
	}
	if version < 1 || version > math.MaxInt16 {
		return nil, fmt.Errorf("vault: the master key version must be 1 to %d", math.MaxInt16)
	}
	master, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	return &Envelope{master: master, version: int16(version)}, nil
}

// Encrypt seals plaintext for the connection under a new data key.
func (e *Envelope) Encrypt(connectionID [16]byte, plaintext []byte) ([]byte, int16, error) {
	dataKey := make([]byte, keySize)
	defer clear(dataKey)
	nonces := make([]byte, 2*nonceSize)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, 0, err
	}
	if _, err := rand.Read(nonces); err != nil {
		return nil, 0, err
	}
	data, err := newGCM(dataKey)
	if err != nil {
		return nil, 0, err
	}
	ad := additionalData(connectionID, e.version)
	keyNonce, secretNonce := nonces[:nonceSize], nonces[nonceSize:]

	block := make([]byte, 0, minBlockSize+len(plaintext))
	block = append(block, formatV1)
	block = append(block, keyNonce...)
	block = e.master.Seal(block, keyNonce, dataKey, ad)
	block = append(block, secretNonce...)
	block = data.Seal(block, secretNonce, plaintext, ad)
	return block, e.version, nil
}

// Decrypt opens a block of the connection. A kek version other than the configured one fails before
// any cryptography. Every failure returns ErrDecrypt and no plaintext.
func (e *Envelope) Decrypt(connectionID [16]byte, block []byte, kekVersion int16) ([]byte, error) {
	if kekVersion != e.version || len(block) < minBlockSize || block[0] != formatV1 {
		return nil, ErrDecrypt
	}
	ad := additionalData(connectionID, kekVersion)
	rest := block[1:]
	keyNonce, rest := rest[:nonceSize], rest[nonceSize:]
	wrapped, rest := rest[:wrappedKeySize], rest[wrappedKeySize:]
	secretNonce, sealed := rest[:nonceSize], rest[nonceSize:]

	dataKey, err := e.master.Open(nil, keyNonce, wrapped, ad)
	if err != nil {
		return nil, ErrDecrypt
	}
	defer clear(dataKey)
	data, err := newGCM(dataKey)
	if err != nil {
		return nil, ErrDecrypt
	}
	plaintext, err := data.Open(nil, secretNonce, sealed, ad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

func additionalData(connectionID [16]byte, kekVersion int16) []byte {
	ad := make([]byte, 0, 18)
	ad = append(ad, connectionID[:]...)
	return binary.BigEndian.AppendUint16(ad, uint16(kekVersion))
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
