// Package evm is the connector of EVM networks (SRS — EVM Connector). C1 holds the wallet address check
// of UC-301 and the allow-list of chain IDs. It has no RPC client and imports nothing of the network.
package evm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"

	"golang.org/x/crypto/sha3"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Connector is the connector of every source of kind EVM.
type Connector struct {
	allowed map[uint64]bool
}

var _ connector.EVMConnector = (*Connector)(nil)

// New returns the connector with the allow-list of chain IDs (EVM_ALLOWED_CHAIN_IDS).
func New(allowedChainIDs []uint64) *Connector {
	allowed := make(map[uint64]bool, len(allowedChainIDs))
	for _, id := range allowedChainIDs {
		allowed[id] = true
	}
	return &Connector{allowed: allowed}
}

// AllowsChain reports whether chainID is in the allow-list.
func (c *Connector) AllowsChain(chainID uint64) bool { return c.allowed[chainID] }

// Capabilities: a wallet has no key and no permissions.
func (c *Connector) Capabilities() connector.Capabilities { return connector.Capabilities{} }

// CheckAccount checks a wallet address (UC-301) and returns it in EIP-55 form, without permissions.
func (c *Connector) CheckAccount(_ context.Context, _ connector.Source, cred connector.Credentials) (connector.AccountInfo, error) {
	if cred.ExchangeKey != nil {
		return connector.AccountInfo{}, &connector.InvalidInputError{Message: "an EVM network takes a wallet, not an exchange key"}
	}
	address, err := CheckAddress(cred.WalletAddress)
	if err != nil {
		return connector.AccountInfo{}, err
	}
	return connector.AccountInfo{Identity: address}, nil
}

// Streams: a wallet connection has no stream in C1 (UC-101 postcondition).
func (c *Connector) Streams(context.Context, connector.Source, connector.AccountInfo) ([]connector.Stream, error) {
	return nil, nil
}

// errNoStreams: the EVM connector of C1 declares no stream, so the engine never asks for pages or snapshots.
var errNoStreams = errors.New("evm: the connector has no streams in C1")

// Budgets: no budget; the RPC client comes in S3.
func (c *Connector) Budgets(connector.Source) []connector.Budget { return nil }

// FetchPage: the connector has no streams.
func (c *Connector) FetchPage(context.Context, connector.Connection, string, connector.Mode, json.RawMessage) (connector.Page, error) {
	return connector.Page{}, errNoStreams
}

// FetchSnapshot: the connector has no streams.
func (c *Connector) FetchSnapshot(context.Context, connector.Connection) (connector.Snapshot, error) {
	return connector.Snapshot{}, errNoStreams
}

// CheckAddress applies UC-301 steps 1 to 3 and returns the address in EIP-55 form.
// The input is not trimmed. The errors are *connector.InvalidInputError and do not quote the input.
func CheckAddress(address string) (string, error) {
	if len(address) != 42 || address[:2] != "0x" || !isHex(address[2:]) {
		return "", &connector.InvalidInputError{Message: "wallet.address must be 0x and 40 hexadecimal characters"}
	}
	digits := address[2:]
	checksummed := ToEIP55(digits)
	if hasLower(digits) && hasUpper(digits) && "0x"+digits != checksummed {
		return "", &connector.InvalidInputError{Message: "wallet.address fails the EIP-55 checksum"}
	}
	if isZero(digits) {
		return "", &connector.InvalidInputError{Message: "wallet.address must not be the zero address"}
	}
	return checksummed, nil
}

// ToEIP55 returns 0x and the 40 hexadecimal digits in EIP-55 mixed case. digits must be hexadecimal.
func ToEIP55(digits string) string {
	lower := []byte(toLower(digits))
	h := sha3.NewLegacyKeccak256()
	h.Write(lower)
	hash := hex.EncodeToString(h.Sum(nil))
	out := make([]byte, 0, 42)
	out = append(out, "0x"...)
	for i, ch := range lower {
		// A letter is upper case when the matching nibble of the hash is 8 or more.
		if ch >= 'a' && ch <= 'f' && hash[i] >= '8' {
			ch -= 'a' - 'A'
		}
		out = append(out, ch)
	}
	return string(out)
}

func isHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func hasLower(s string) bool {
	for i := range len(s) {
		if s[i] >= 'a' && s[i] <= 'f' {
			return true
		}
	}
	return false
}

func hasUpper(s string) bool {
	for i := range len(s) {
		if s[i] >= 'A' && s[i] <= 'F' {
			return true
		}
	}
	return false
}

func isZero(s string) bool {
	for i := range len(s) {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'F' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
