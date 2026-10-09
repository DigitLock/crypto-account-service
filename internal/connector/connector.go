// Package connector is the contract between the core and the source adapters (ADR-2, SRS — Core §2.1.1
// Connector contract): capabilities, the account check, the declared streams, the budgets of the rate
// limiter, pages of entries and balance snapshots. Amounts are decimal strings: no float on the path.
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

// Source is a row of sources as a connector sees it, with the alias rows of the source (S3 D-31). The engine and
// CreateConnection load them; a connector never reads the database.
type Source struct {
	Code    string
	Kind    string
	Enabled bool
	Config  json.RawMessage
	Aliases []Alias
}

// Alias is a row of asset_aliases: a native asset code or token address of the source and its canonical asset.
type Alias struct {
	NativeAsset string
	Asset       string
	Decimals    *int16 // nil when the row has none
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
	// IPRestricted reports whether the source restricts the key to IP addresses; nil when the source cannot tell
	// (X1 D-2).
	IPRestricted *bool
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
	// CheckAccount checks an exchange key or a wallet address on a source and returns the account. A request
	// to the source reserves its cost in lim first (FR-110). Used by CreateConnection and the periodic key check.
	CheckAccount(ctx context.Context, src Source, cred Credentials, lim Limiter) (AccountInfo, error)
	// Streams declares the streams of a connection of the account.
	Streams(ctx context.Context, src Source, account AccountInfo) ([]Stream, error)
	// Budgets declares the budgets of the rate limiter of a source. The engine builds one limiter per source.
	Budgets(src Source) []Budget
	// FetchPage returns one page of a ledger stream from cursor. Only final records are returned.
	FetchPage(ctx context.Context, conn Connection, stream string, mode Mode, cursor json.RawMessage) (Page, error)
	// FetchSnapshot returns all balances of the connection and their time, or fails as a whole.
	FetchSnapshot(ctx context.Context, conn Connection) (Snapshot, error)
}

// Budget is a number of cost units per time window of one source.
type Budget struct {
	Name   string
	Units  int
	Window time.Duration
}

// Limiter is the rate limiter of a source, as a connector sees it.
type Limiter interface {
	// Reserve takes cost units of a budget before a request; it waits until the budget allows it and
	// returns the error of ctx when ctx ends first.
	Reserve(ctx context.Context, budget string, cost int) error
	// Pause blocks a budget for the pause the source demands; the other budgets go on.
	Pause(budget string, d time.Duration)
	// Observe reports the units the source counts as used in a budget, as a header of its answer tells: the budget
	// then counts at least used units in its current window (X1 D-7).
	Observe(budget string, used int)
}

// Connection is what the page and snapshot calls get.
type Connection struct {
	// ID is the connection ID in its 36-character form.
	ID     string
	Source Source
	// Account is the account identity: an exchange account ID or a wallet address.
	Account string
	// Key is the decrypted key of an exchange connection, for this call only; nil for a wallet.
	Key *ExchangeKey
	// Limiter is the limiter of the source; every request reserves its cost in it first.
	Limiter Limiter
}

// Page is one page of a ledger stream.
type Page struct {
	Entries []Entry
	// Cursor and Mode are where the next page starts.
	Cursor json.RawMessage
	Mode   Mode
	// More reports that more pages follow now.
	More bool
	// Checkpoint is the optional balance checkpoint of the account where the records of the page end (S3 D-3).
	// nil: none. The ledger writer compares it with the ledger in the transaction of the page.
	Checkpoint *Checkpoint
}

// Checkpoint is a balance checkpoint: the balances of the account at the point the records of a page end, one per
// native asset, by the rules of a snapshot balance (SRS — Core Connector contract; EVM: UC-304).
type Checkpoint struct {
	// BlockNumber and BlockHash are the block of an EVM checkpoint; nil and empty for other sources.
	BlockNumber *uint64
	BlockHash   string
	// TakenAt is the time of the checkpoint at the source: the block time on EVM.
	TakenAt  time.Time
	Balances []Balance
}

// Entry is one movement of one asset, as the connector maps it (SRS — Core §2.1.4, ADR-5).
type Entry struct {
	ExternalID  string
	Leg         string
	Type        string
	Direction   string
	NativeAsset string
	// Amount is a positive plain decimal: at most 20 integer digits and 18 decimal places.
	Amount     string
	OccurredAt time.Time
	// Raw is the source record as received.
	Raw json.RawMessage
}

// Snapshot is all balances of a connection at one time.
type Snapshot struct {
	TakenAt  time.Time
	Balances []Balance
}

// Balance is the balance of one asset in one account type. Free and Locked are plain decimals, not negative.
type Balance struct {
	AccountType string
	NativeAsset string
	Free        string
	Locked      string
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
	// Budget is the budget the source limited; the connector has reported the pause to the limiter.
	Budget string
	Pause  time.Duration
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
