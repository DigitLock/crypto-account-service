package binance

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Defaults of the Binance values of sources.config (SRS — Binance §3.1).
const (
	DefaultBaseURL          = "https://api.binance.com"
	DefaultBudgetShare      = 0.5
	DefaultRecvWindowMS     = 5000
	DefaultTimeSyncInterval = time.Hour
	// MaxRecvWindowMS is the largest recvWindow Binance accepts (SRS — Binance §2.1.1 Time).
	MaxRecvWindowMS = 60000
)

// FamilyBalances is the stream family of the balance snapshot.
const FamilyBalances = "balances"

// Config is the Binance part of sources.config of the binance source (SRS — Binance §3.1). The values of X2 and
// W1 (sync_interval.ledger, finality_lookback, backfill_floor, discovery_interval, events.*) come with them.
type Config struct {
	// BaseURL has a scheme and a host and no trailing slash.
	BaseURL string
	// BudgetShare is the part of each limit the connector may use: above 0, at most 1. A share of a limit, not an
	// amount.
	BudgetShare float64
	// RecvWindowMS is the recvWindow of signed requests: 1 … MaxRecvWindowMS.
	RecvWindowMS int
	// TimeSyncInterval is the age after which the time offset is read again before a signed request (X1 D-8).
	TimeSyncInterval time.Duration
	// BalancesInterval is sync_interval.balances: 15 min by default (SRS — Core §3.1).
	BalancesInterval time.Duration
}

// ParseConfig parses the Binance part of sources.config of src. Every value has a default: a missing or malformed
// one takes it (SRS — Core §3.1), so the parse never fails. No request.
func ParseConfig(src connector.Source) Config {
	cfg := Config{
		BaseURL:          DefaultBaseURL,
		BudgetShare:      DefaultBudgetShare,
		RecvWindowMS:     DefaultRecvWindowMS,
		TimeSyncInterval: DefaultTimeSyncInterval,
		BalancesInterval: connector.SyncInterval(src, FamilyBalances),
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(src.Config, &raw) != nil {
		return cfg
	}
	if s, ok := stringOf(raw["base_url"]); ok {
		if u, ok := baseURL(s); ok {
			cfg.BaseURL = u
		}
	}
	// A JSON number, not a string.
	if share, err := strconv.ParseFloat(string(raw["budget_share"]), 64); err == nil && share > 0 && share <= 1 {
		cfg.BudgetShare = share
	}
	// A JSON integer: not a string, not a fraction, not an exponent.
	if n, err := strconv.ParseUint(string(raw["recv_window_ms"]), 10, 32); err == nil && n >= 1 && n <= MaxRecvWindowMS {
		cfg.RecvWindowMS = int(n)
	}
	if s, ok := stringOf(raw["time_sync_interval"]); ok {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			cfg.TimeSyncInterval = d
		}
	}
	return cfg
}

// baseURL accepts an absolute http or https URL with a host and without user info, query, fragment or path, and
// returns it without a trailing slash.
func baseURL(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	return strings.TrimSuffix(s, "/"), true
}

func stringOf(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}
