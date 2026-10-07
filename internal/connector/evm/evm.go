// Package evm is the connector of EVM networks (SRS — EVM Connector). C1 holds the wallet address check of
// UC-301 and the allow-list of chain IDs; S3 adds the parsing of sources.config (config.go), the RPC access with
// one endpoint per run (rpc.go), the start checks (checks.go) and the log stream (logs.go). It holds no key and
// sends no transaction (ADR-3, FR-313): read methods only.
package evm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/crypto/sha3"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Connector is the connector of every source of kind EVM.
type Connector struct {
	allowed   map[uint64]bool
	endpoints map[string]Endpoints // by source code
	metrics   Metrics
	logger    *slog.Logger

	mu       sync.Mutex
	networks map[string]*network // by source code
}

var _ connector.EVMConnector = (*Connector)(nil)

// New returns the connector with the allow-list of chain IDs (EVM_ALLOWED_CHAIN_IDS), the endpoints of
// EVM_RPC_URL_<SOURCE> and EVM_RPC_FALLBACK_URL_<SOURCE> by source code, the sink of its metrics and its logger.
// No client is dialled here: a source gets its clients at its first run. metrics and logger may be nil.
func New(allowedChainIDs []uint64, endpoints map[string]Endpoints, metrics Metrics, logger *slog.Logger) *Connector {
	allowed := make(map[uint64]bool, len(allowedChainIDs))
	for _, id := range allowedChainIDs {
		allowed[id] = true
	}
	if metrics == nil {
		metrics = nopMetrics{}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Connector{allowed: allowed, endpoints: endpoints, metrics: metrics, logger: logger,
		networks: map[string]*network{}}
}

// AllowsChain reports whether chainID is in the allow-list.
func (c *Connector) AllowsChain(chainID uint64) bool { return c.allowed[chainID] }

// HasEndpoint reports whether EVM_RPC_URL_<SOURCE> of the source is set. Without it the source declares no
// streams (S3 D-16).
func (c *Connector) HasEndpoint(code string) bool { return c.endpoints[code].Primary.Value() != "" }

// Capabilities: a wallet has no key and no permissions.
func (c *Connector) Capabilities() connector.Capabilities { return connector.Capabilities{} }

// CheckAccount checks a wallet address (UC-301) and returns it in EIP-55 form, without permissions. The check
// is local: no request, so no reservation in the limiter (C1-T534).
func (c *Connector) CheckAccount(_ context.Context, _ connector.Source, cred connector.Credentials, _ connector.Limiter) (connector.AccountInfo, error) {
	if cred.ExchangeKey != nil {
		return connector.AccountInfo{}, &connector.InvalidInputError{Message: "an EVM network takes a wallet, not an exchange key"}
	}
	address, err := CheckAddress(cred.WalletAddress)
	if err != nil {
		return connector.AccountInfo{}, err
	}
	return connector.AccountInfo{Identity: address}, nil
}

// Streams declares balances and logs of a wallet connection when the source has a primary endpoint, and none
// otherwise (S3 D-16). No RPC call. The logs stream starts in BACKFILL with an empty cursor: the first run reads
// backfill_floor, so a floor set after the engine started is used (S3 D-36).
func (c *Connector) Streams(_ context.Context, src connector.Source, _ connector.AccountInfo) ([]connector.Stream, error) {
	if !c.HasEndpoint(src.Code) {
		return nil, nil
	}
	balances, logs := Intervals(src)
	empty := json.RawMessage(`{}`)
	return []connector.Stream{
		{Name: FamilyBalances, Family: FamilyBalances, Interval: balances, FirstMode: connector.ModeIncremental, FirstCursor: empty},
		{Name: FamilyLogs, Family: FamilyLogs, Interval: logs, FirstMode: connector.ModeBackfill, FirstCursor: empty},
	}, nil
}

// Budgets: one budget per endpoint, rpc_rate_limit requests per second each (S3 D-12). A paused primary does not
// block the fallback (§3.2 Reliability).
func (c *Connector) Budgets(src connector.Source) []connector.Budget {
	n := RPCRateLimit(src)
	return []connector.Budget{
		{Name: EndpointPrimary, Units: n, Window: time.Second},
		{Name: EndpointFallback, Units: n, Window: time.Second},
	}
}

// ErrNotBuilt is the end of a run at the reads that later stages of S3 build: the log filters and the balances.
var ErrNotBuilt = errors.New("evm: not built in st3")

// FetchPage runs the logs stream (UC-303).
func (c *Connector) FetchPage(ctx context.Context, conn connector.Connection, stream string, mode connector.Mode, cursor json.RawMessage) (connector.Page, error) {
	if stream != FamilyLogs {
		return connector.Page{}, fmt.Errorf("evm: unknown stream %q", stream)
	}
	cur, err := parseCursor(cursor)
	if err != nil {
		return connector.Page{}, err
	}
	s, err := c.open(ctx, conn)
	var page connector.Page
	if err == nil {
		page, err = s.logs(ctx, mode, cur, cursor)
	}
	s.close(err)
	return page, err
}

// FetchSnapshot runs the balances stream (UC-302): the endpoint choice and the start checks of this stage; the
// balance reads come in st4.
func (c *Connector) FetchSnapshot(ctx context.Context, conn connector.Connection) (connector.Snapshot, error) {
	s, err := c.open(ctx, conn)
	if err == nil {
		err = fmt.Errorf("%w: balances of UC-302 steps 2 to 4", ErrNotBuilt)
	}
	s.close(err)
	return connector.Snapshot{}, err
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
