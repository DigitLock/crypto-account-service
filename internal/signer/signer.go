// Package signer signs the transactions of the operator of card-auth (ADR-10). It is the only package that
// holds the operator key; packages of server must not import it (ADR-3, test S2-T709).
package signer

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Signer signs transactions as one account. A KMS or HSM implementation can replace KeySigner
// without touching the callers.
type Signer interface {
	Address() common.Address
	SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}

// KeySigner signs with a private key from the environment (OPERATOR_PRIVATE_KEY).
// Every fmt verb, String, GoString, LogValue and text marshalling yield "[redacted]".
type KeySigner struct {
	key     *ecdsa.PrivateKey
	address common.Address
}

var _ Signer = (*KeySigner)(nil)

// ErrInvalidKey is returned by New for bytes that are not a valid secp256k1 private key.
var ErrInvalidKey = errors.New("signer: OPERATOR_PRIVATE_KEY is not a valid secp256k1 private key")

// New returns the signer of the 32-byte key. The error never contains the key.
func New(key vault.Secret[[]byte]) (*KeySigner, error) {
	// The crypto error is not wrapped: it may describe the value.
	k, err := crypto.ToECDSA(key.Value())
	if err != nil {
		return nil, ErrInvalidKey
	}
	return &KeySigner{key: k, address: crypto.PubkeyToAddress(k.PublicKey)}, nil
}

// Address returns the account of the key.
func (s *KeySigner) Address() common.Address { return s.address }

// SignTx signs tx for chainID with the latest signer of that chain (EIP-1559 and earlier types).
func (s *KeySigner) SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	if chainID == nil || chainID.Sign() <= 0 {
		return nil, errors.New("signer: chain ID must be positive")
	}
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), s.key)
	if err != nil {
		return nil, fmt.Errorf("signer: sign the transaction: %w", err)
	}
	return signed, nil
}

const redacted = "[redacted]"

func (KeySigner) String() string               { return redacted }
func (KeySigner) GoString() string             { return redacted }
func (KeySigner) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (KeySigner) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (KeySigner) MarshalText() ([]byte, error) { return []byte(redacted), nil }
