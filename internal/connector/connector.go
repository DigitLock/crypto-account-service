// Package connector is the contract between the core and the source adapters (ADR-2, SRS — Core §2.1.1
// Connector contract). This stage holds what connection creation needs: capabilities, the account check
// and the declared streams. Pages, snapshots, the rate limiter and the periodic key check come in st7.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Kinds of a source (SRS — Core §2.4 sources.kind).
const (
	KindExchange = "EXCHANGE"
	KindEVM      = "EVM"
)

// Mode is the mode of a stream (SRS — Core §2.4 sync_cursors.mode).
type Mode string

const (
	ModeBackfill    Mode = "BACKFILL"
	ModeIncremental Mode = "INCREMENTAL"
)

// PermissionRead is the only permission a key may hold (EC-101).
const PermissionRead = "READ"

// Capabilities are the flags a connector declares. C1 reads one.
type Capabilities struct {
	// PermissionsReadable: the account check reports the permissions of a key (EC-103).
	PermissionsReadable bool
}

// Source is a row of sources as a connector sees it.
type Source struct {
	Code    string
	Kind    string
	Enabled bool
	Config  json.RawMessage
}

// ExchangeKey is an API key and its secret. Both print as redacted in every fmt verb, in slog and in JSON.
type ExchangeKey struct {
	APIKey    vault.Secret[string]
	APISecret vault.Secret[string]
}

// Credentials are what the account check needs: an exchange key or a wallet address, never both.
type Credentials struct {
	ExchangeKey   *ExchangeKey
	WalletAddress string
}

// AccountInfo is the result of the account check.
type AccountInfo struct {
	// Identity is the account at the source: an exchange account ID or a wallet address in EIP-55 form.
	Identity string
	// Permissions of a key as reported by the source. Empty for a wallet.
	Permissions []string
}

// Stream is a stream a connector declares for a connection.
type Stream struct {
	// Name is the full stream name: balances, trades:BTCUSDT, logs.
	Name string
	// Family is the stream family: balances, trades, logs.
	Family string
	// Interval is the sync period, read by the connector from sources.config.
	Interval    time.Duration
	FirstMode   Mode
	FirstCursor json.RawMessage
}

// Connector is a source adapter.
type Connector interface {
	Capabilities() Capabilities
	// CheckAccount checks an exchange key or a wallet address on a source and returns the account.
	CheckAccount(ctx context.Context, src Source, cred Credentials) (AccountInfo, error)
	// Streams declares the streams of a connection of the account.
	Streams(ctx context.Context, src Source, account AccountInfo) ([]Stream, error)
}

// ChainAllowList is implemented by the EVM connector, which holds the allow-list of chain IDs.
type ChainAllowList interface {
	AllowsChain(chainID uint64) bool
}

// EVMConnector is the connector of every source of kind EVM.
type EVMConnector interface {
	Connector
	ChainAllowList
}

// Typed errors of a connector. Anything else is a plain failure.
var (
	// ErrKeyRejected: the source rejects the key.
	ErrKeyRejected = errors.New("connector: the source rejects the key")
	// ErrUnreachable: the source cannot be reached.
	ErrUnreachable = errors.New("connector: the source cannot be reached")
)

// KeyNotReadOnlyError: the key holds permissions beyond reading.
type KeyNotReadOnlyError struct {
	Permissions []string
}

func (e *KeyNotReadOnlyError) Error() string {
	return "connector: the key is not read-only: " + strings.Join(e.Permissions, ", ")
}

// RateLimitError: the source answered with a rate limit and demands a pause.
type RateLimitError struct {
	Pause time.Duration
}

func (e *RateLimitError) Error() string {
	return "connector: rate limit of the source, pause " + e.Pause.String()
}

// InvalidInputError: the input does not suit the source. Message is safe to return to the caller.
type InvalidInputError struct {
	Message string
}

func (e *InvalidInputError) Error() string { return e.Message }

// Set holds the connectors: one per source code, and one EVM connector for every source of kind EVM.
type Set struct {
	byCode map[string]Connector
	evm    EVMConnector
}

// NewSet returns an empty set.
func NewSet() *Set {
	return &Set{byCode: map[string]Connector{}}
}

// Register registers c under a source code.
func (s *Set) Register(code string, c Connector) {
	s.byCode[code] = c
}

// RegisterEVM registers the connector of every source of kind EVM.
func (s *Set) RegisterEVM(c EVMConnector) {
	s.evm = c
}

// For returns the connector of a source: the one of its code, else for kind EVM the EVM connector.
func (s *Set) For(src Source) (Connector, bool) {
	if c, ok := s.byCode[src.Code]; ok {
		return c, true
	}
	if src.Kind == KindEVM && s.evm != nil {
		return s.evm, true
	}
	return nil, false
}

// Available decides whether a source is offered (SRS — Core §2.1.1 Common rules): it is enabled, a
// connector exists and, for kind EVM, the chain_id of its config is in the allow-list. A missing or
// malformed chain_id means not available.
func (s *Set) Available(src Source) bool {
	if !src.Enabled {
		return false
	}
	if _, ok := s.For(src); !ok {
		return false
	}
	if src.Kind != KindEVM {
		return true
	}
	chainID, ok := ChainID(src)
	return ok && s.evm != nil && s.evm.AllowsChain(chainID)
}

// ChainID returns the chain_id of a source's config when it is a positive JSON integer: not a string,
// not a fraction, not zero.
func ChainID(src Source) (uint64, bool) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal(src.Config, &config); err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(string(config["chain_id"]), 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
}
