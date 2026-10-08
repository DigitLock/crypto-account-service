package evm

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Finality modes and tags of sources.config (§2.1.1 Finality rule).
const (
	FinalityModeTag           = chain.FinalityModeTag
	FinalityModeConfirmations = chain.FinalityModeConfirmations
	FinalityTagFinalized      = chain.FinalityTagFinalized
	FinalityTagSafe           = chain.FinalityTagSafe
)

// Defaults of the EVM values of sources.config (SRS — EVM Connector §3.1).
const (
	DefaultFinalityTag           = FinalityTagFinalized
	DefaultFinalityConfirmations = 10
	DefaultLogRangeMax           = 2000
	DefaultRPCRateLimit          = 5 // requests per second and endpoint
	DefaultCompletenessInterval  = time.Hour
	DefaultLogsInterval          = 5 * time.Minute
)

// Stream families of the connector.
const (
	FamilyBalances = "balances"
	FamilyLogs     = "logs"
)

// Config is the EVM part of sources.config of one source (SRS — EVM Connector §3.1).
type Config struct {
	ChainID               uint64
	FinalityMode          string // FinalityModeTag or FinalityModeConfirmations
	FinalityTag           string // used in mode tag
	FinalityConfirmations uint64 // used in mode confirmations
	ControllerAddress     string // EIP-55
	TreasuryConnection    string // UUID; empty until set
	TreasuryAddress       string // EIP-55; empty until set (S3 D-32)
	BackfillFloor         uint64
	LogRangeMax           uint64
	RPCRateLimit          int
	CompletenessInterval  time.Duration
	BalancesInterval      time.Duration
	LogsInterval          time.Duration
}

// Rule is the finality rule of the network for the shared read of internal/chain.
func (c Config) Rule() chain.FinalityRule {
	return chain.FinalityRule{Mode: c.FinalityMode, Tag: c.FinalityTag, Confirmations: c.FinalityConfirmations}
}

// ErrNetworkConfig marks a value of sources.config that makes the network unusable: a missing or malformed
// chain_id, finality_mode, controller_address or backfill_floor (S3 D-30, S3 D-36). The errors name the key,
// never the value.
var ErrNetworkConfig = errors.New("evm: sources.config")

type configError struct{ msg string }

func (e *configError) Error() string { return "evm: sources.config: " + e.msg }
func (e *configError) Unwrap() error { return ErrNetworkConfig }

// ParseConfig parses the EVM part of sources.config of src. chain_id, finality_mode, controller_address and
// backfill_floor have no default: a missing or malformed one is an error that wraps ErrNetworkConfig. Any other
// malformed value takes its default, or stays unset when it has none, as the rule of SRS — Core §3.1. No RPC call.
func ParseConfig(src connector.Source) (Config, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(src.Config, &raw); err != nil {
		return Config{}, &configError{"not a JSON object"}
	}
	chainID, ok := connector.ChainID(src)
	if !ok {
		return Config{}, &configError{"chain_id must be a positive integer"}
	}
	balances, logs := Intervals(src)
	cfg := Config{
		ChainID:               chainID,
		FinalityTag:           DefaultFinalityTag,
		FinalityConfirmations: DefaultFinalityConfirmations,
		LogRangeMax:           DefaultLogRangeMax,
		RPCRateLimit:          RPCRateLimit(src),
		CompletenessInterval:  DefaultCompletenessInterval,
		BalancesInterval:      balances,
		LogsInterval:          logs,
	}

	switch mode, _ := stringOf(raw["finality_mode"]); mode {
	case FinalityModeTag, FinalityModeConfirmations:
		cfg.FinalityMode = mode
	default:
		return Config{}, &configError{"finality_mode must be tag or confirmations"}
	}
	s, _ := stringOf(raw["controller_address"])
	address, err := CheckAddress(s)
	if err != nil {
		return Config{}, &configError{"controller_address must be set to an address: casctl source set"}
	}
	cfg.ControllerAddress = address
	n, ok := uintOf(raw["backfill_floor"])
	if !ok {
		return Config{}, &configError{"backfill_floor must be set to a block number: casctl source set"}
	}
	cfg.BackfillFloor = n

	if tag, _ := stringOf(raw["finality_tag"]); tag == FinalityTagFinalized || tag == FinalityTagSafe {
		cfg.FinalityTag = tag
	}
	if n, ok := uintOf(raw["finality_confirmations"]); ok && n >= 1 {
		cfg.FinalityConfirmations = n
	}
	if s, ok := stringOf(raw["treasury_connection"]); ok && isUUID(s) {
		cfg.TreasuryConnection = s
	}
	if s, ok := stringOf(raw["treasury_address"]); ok {
		if address, err := CheckAddress(s); err == nil {
			cfg.TreasuryAddress = address
		}
	}
	if n, ok := uintOf(raw["log_range_max"]); ok && n >= 1 {
		cfg.LogRangeMax = n
	}
	if d, ok := durationOf(raw["completeness_interval"]); ok {
		cfg.CompletenessInterval = d
	}
	return cfg, nil
}

// Intervals are the sync intervals of the two streams: balances by connector.SyncInterval (15 min), logs 5 min
// unless sync_interval.logs sets a valid one: the EVM default, not the 1 h of SRS — Core §3.1. They do not depend
// on the values without default, so the streams are declared for a network whose config is incomplete and its
// runs fail by the start checks (S3 D-30).
func Intervals(src connector.Source) (balances, logs time.Duration) {
	balances, logs = connector.SyncInterval(src, FamilyBalances), DefaultLogsInterval
	var raw struct {
		SyncInterval map[string]json.RawMessage `json:"sync_interval"`
	}
	if json.Unmarshal(src.Config, &raw) == nil {
		if d, ok := durationOf(raw.SyncInterval[FamilyLogs]); ok {
			logs = d
		}
	}
	return balances, logs
}

// RPCRateLimit is rpc_rate_limit of the source: requests per second and endpoint, a JSON integer ≥ 1, default 5.
func RPCRateLimit(src connector.Source) int {
	var raw map[string]json.RawMessage
	if json.Unmarshal(src.Config, &raw) != nil {
		return DefaultRPCRateLimit
	}
	if n, ok := uintOf(raw["rpc_rate_limit"]); ok && n >= 1 && n <= 1<<31-1 {
		return int(n)
	}
	return DefaultRPCRateLimit
}

func stringOf(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// uintOf accepts a JSON integer ≥ 0: not a string, not a fraction, not an exponent.
func uintOf(raw json.RawMessage) (uint64, bool) {
	n, err := strconv.ParseUint(string(raw), 10, 64)
	return n, err == nil
}

// durationOf accepts a positive duration string such as "1h".
func durationOf(raw json.RawMessage) (time.Duration, bool) {
	s, ok := stringOf(raw)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d > 0
}

// uuidForm is the 36-character form of a UUID (SRS — Core §2.1.1 IDs). Not github.com/google/uuid: it imports
// net, and this package imports nothing of the network (C1-T534).
var uuidForm = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidForm.MatchString(s) }
