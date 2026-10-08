// Package config loads the environment parameters of server (SRS — Core §3.1) and of card-auth
// (SRS — Card Spend §3.1). Parameters stored in sources.config are not part of this package.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// MasterKeySize is the length of CAS_MASTER_KEY after base64 decoding (ADR-4).
const MasterKeySize = 32

// Config is the parsed environment of server.
// CAS_MASTER_KEY, DATABASE_URL and the RPC URLs are held as vault.Secret values: no method prints them,
// and LogValue omits them.
type Config struct {
	MasterKey        vault.Secret[[]byte]
	MasterKeyVersion int
	DatabaseURL      vault.Secret[string]

	DBPoolMaxConns int32
	DBPoolMinConns int32

	GRPCPort       int
	HealthHTTPPort int

	LogLevel  slog.Level
	LogFormat string

	ShutdownTimeout time.Duration

	SyncTick             time.Duration
	SyncWorkers          int
	SyncMaxPagesPerRun   int
	SyncLockRetry        time.Duration
	SyncFailureThreshold int
	SyncBackoffInitial   time.Duration
	SyncBackoffMax       time.Duration
	TriggerSyncCooldown  time.Duration
	KeyCheckInterval     time.Duration

	EnableFakeSource bool

	EVMAllowedChainIDs []uint64

	// EVMRPC holds the endpoints of EVM_RPC_URL_<SOURCE> and EVM_RPC_FALLBACK_URL_<SOURCE> by source code
	// (SRS — EVM Connector §3.1). A source without a variable has no entry.
	EVMRPC map[string]EVMEndpoints
}

// EVMEndpoints are the RPC endpoints of one EVM source, as the EVM connector takes them.
type EVMEndpoints = evm.Endpoints

// Prefixes of the endpoint variables of an EVM source; the suffix is the source code in upper case with _ for -.
const (
	EVMRPCURLPrefix         = evm.RPCURLPrefix
	EVMRPCFallbackURLPrefix = evm.RPCFallbackURLPrefix
)

// Log formats of LOG_FORMAT.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Load reads and validates the environment through getenv. An empty value counts as unset. names are the
// names of the variables of the environment: the endpoint variables of the EVM sources are found among them.
// The returned error names every invalid variable and never contains a value.
func Load(getenv func(string) string, names ...string) (Config, error) {
	p := parser{getenv: getenv}
	cfg := Config{
		MasterKey:        p.masterKey("CAS_MASTER_KEY"),
		MasterKeyVersion: p.integer("CAS_MASTER_KEY_VERSION", 1, 1, math.MaxInt16), // kek_version is SMALLINT
		DatabaseURL:      vault.NewSecret(p.required("DATABASE_URL")),

		DBPoolMaxConns: int32(p.integer("DB_POOL_MAX_CONNS", 10, 1, 1<<31-1)),
		DBPoolMinConns: int32(p.integer("DB_POOL_MIN_CONNS", 2, 0, 1<<31-1)),

		GRPCPort:       p.integer("GRPC_PORT", 50053, 1, 65535),
		HealthHTTPPort: p.integer("HEALTH_HTTP_PORT", 8091, 1, 65535),

		LogLevel:  p.logLevel("LOG_LEVEL", slog.LevelInfo),
		LogFormat: p.oneOf("LOG_FORMAT", LogFormatJSON, LogFormatJSON, LogFormatText),

		ShutdownTimeout: p.duration("SHUTDOWN_TIMEOUT", 15*time.Second),

		SyncTick:             p.duration("SYNC_TICK", time.Second),
		SyncWorkers:          p.integer("SYNC_WORKERS", 4, 1, 1<<31-1),
		SyncMaxPagesPerRun:   p.integer("SYNC_MAX_PAGES_PER_RUN", 20, 1, 1<<31-1),
		SyncLockRetry:        p.duration("SYNC_LOCK_RETRY", 10*time.Second),
		SyncFailureThreshold: p.integer("SYNC_FAILURE_THRESHOLD", 5, 1, 1<<31-1),
		SyncBackoffInitial:   p.duration("SYNC_BACKOFF_INITIAL", 30*time.Second),
		SyncBackoffMax:       p.duration("SYNC_BACKOFF_MAX", time.Hour),
		TriggerSyncCooldown:  p.duration("TRIGGER_SYNC_COOLDOWN", 60*time.Second),
		KeyCheckInterval:     p.duration("KEY_CHECK_INTERVAL", 24*time.Hour),

		EnableFakeSource: p.boolean("ENABLE_FAKE_SOURCE", false),

		EVMAllowedChainIDs: p.chainIDs("EVM_ALLOWED_CHAIN_IDS", []uint64{31337, 84532}),

		EVMRPC: p.evmEndpoints(names),
	}

	if cfg.DBPoolMinConns > cfg.DBPoolMaxConns {
		p.fail("DB_POOL_MIN_CONNS", "must not exceed DB_POOL_MAX_CONNS")
	}
	if cfg.SyncBackoffInitial > cfg.SyncBackoffMax {
		p.fail("SYNC_BACKOFF_INITIAL", "must not exceed SYNC_BACKOFF_MAX")
	}

	if err := errors.Join(p.errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LogValue implements slog.LogValuer. CAS_MASTER_KEY and DATABASE_URL are omitted; an RPC URL is shown as set
// or unset only.
func (c Config) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.Int("CAS_MASTER_KEY_VERSION", c.MasterKeyVersion),
		slog.Int("DB_POOL_MAX_CONNS", int(c.DBPoolMaxConns)),
		slog.Int("DB_POOL_MIN_CONNS", int(c.DBPoolMinConns)),
		slog.Int("GRPC_PORT", c.GRPCPort),
		slog.Int("HEALTH_HTTP_PORT", c.HealthHTTPPort),
		slog.String("LOG_LEVEL", strings.ToLower(c.LogLevel.String())),
		slog.String("LOG_FORMAT", c.LogFormat),
		duration("SHUTDOWN_TIMEOUT", c.ShutdownTimeout),
		duration("SYNC_TICK", c.SyncTick),
		slog.Int("SYNC_WORKERS", c.SyncWorkers),
		slog.Int("SYNC_MAX_PAGES_PER_RUN", c.SyncMaxPagesPerRun),
		duration("SYNC_LOCK_RETRY", c.SyncLockRetry),
		slog.Int("SYNC_FAILURE_THRESHOLD", c.SyncFailureThreshold),
		duration("SYNC_BACKOFF_INITIAL", c.SyncBackoffInitial),
		duration("SYNC_BACKOFF_MAX", c.SyncBackoffMax),
		duration("TRIGGER_SYNC_COOLDOWN", c.TriggerSyncCooldown),
		duration("KEY_CHECK_INTERVAL", c.KeyCheckInterval),
		slog.Bool("ENABLE_FAKE_SOURCE", c.EnableFakeSource),
		slog.String("EVM_ALLOWED_CHAIN_IDS", joinUint(c.EVMAllowedChainIDs)),
	}
	for _, code := range sortedKeys(c.EVMRPC) {
		e := c.EVMRPC[code]
		attrs = append(attrs,
			setOrUnset(EVMRPCURLPrefix+EVMSourceSuffix(code), e.Primary),
			setOrUnset(EVMRPCFallbackURLPrefix+EVMSourceSuffix(code), e.Fallback))
	}
	return slog.GroupValue(attrs...)
}

func setOrUnset(key string, v vault.Secret[string]) slog.Attr {
	if v.Value() == "" {
		return slog.String(key, "unset")
	}
	return slog.String(key, "set")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EVMSourceSuffix returns the suffix of the endpoint variables of a source: base-sepolia → BASE_SEPOLIA.
func EVMSourceSuffix(code string) string { return evm.SourceSuffix(code) }

// evmSourceCode returns the source code of a suffix, BASE_SEPOLIA → base-sepolia, or false when the suffix is
// not upper-case letters, digits and _.
func evmSourceCode(suffix string) (string, bool) {
	if suffix == "" {
		return "", false
	}
	for _, r := range suffix {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return "", false
		}
	}
	return strings.ReplaceAll(strings.ToLower(suffix), "_", "-"), true
}

// evmEndpoints reads every EVM_RPC_URL_<SOURCE> and EVM_RPC_FALLBACK_URL_<SOURCE> among names: an http or https
// URL with a host. A name with another suffix fails, so a misspelt variable does not leave a source without its
// endpoint unnoticed. An empty value counts as unset.
func (p *parser) evmEndpoints(names []string) map[string]EVMEndpoints {
	out := map[string]EVMEndpoints{}
	for _, name := range slices.Sorted(slices.Values(names)) {
		var suffix string
		var fallback bool
		switch {
		case strings.HasPrefix(name, EVMRPCFallbackURLPrefix):
			suffix, fallback = strings.TrimPrefix(name, EVMRPCFallbackURLPrefix), true
		case strings.HasPrefix(name, EVMRPCURLPrefix):
			suffix = strings.TrimPrefix(name, EVMRPCURLPrefix)
		default:
			continue
		}
		code, ok := evmSourceCode(suffix)
		if !ok {
			p.fail(name, "must end with a source code in upper case with _ for -, such as BASE_SEPOLIA")
			continue
		}
		u := p.url(name, false, "http", "https")
		if u.Value() == "" {
			continue
		}
		e := out[code]
		if fallback {
			e.Fallback = u
		} else {
			e.Primary = u
		}
		out[code] = e
	}
	return out
}

// duration logs d in Go syntax, such as 15s or 24h0m0s, not in nanoseconds.
func duration(key string, d time.Duration) slog.Attr {
	return slog.String(key, d.String())
}

// parser collects one error per invalid variable. Messages name the variable, never the value.
type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(name, rule string) {
	p.errs = append(p.errs, fmt.Errorf("config: %s %s", name, rule))
}

func (p *parser) required(name string) string {
	v := p.getenv(name)
	if v == "" {
		p.fail(name, "is required")
	}
	return v
}

func (p *parser) masterKey(name string) vault.Secret[[]byte] {
	v := p.required(name)
	if v == "" {
		return vault.Secret[[]byte]{}
	}
	// The decoding error is not wrapped: it would point into the value.
	key, err := base64.StdEncoding.Strict().DecodeString(v)
	if err != nil || len(key) != MasterKeySize {
		p.fail(name, fmt.Sprintf("must be standard base64 of exactly %d bytes", MasterKeySize))
		return vault.Secret[[]byte]{}
	}
	return vault.NewSecret(key)
}

func (p *parser) integer(name string, def, minimum, maximum int) int {
	v := p.getenv(name)
	if v == "" {
		return def
	}
	// The strconv error is not wrapped: it quotes the value.
	n, err := strconv.Atoi(v)
	if err != nil || n < minimum || n > maximum {
		p.fail(name, fmt.Sprintf("must be an integer from %d to %d", minimum, maximum))
		return def
	}
	return n
}

func (p *parser) duration(name string, def time.Duration) time.Duration {
	v := p.getenv(name)
	if v == "" {
		return def
	}
	// The time error is not wrapped: it quotes the value.
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.fail(name, "must be a positive Go duration such as 30s or 1h")
		return def
	}
	return d
}

func (p *parser) boolean(name string, def bool) bool {
	switch p.getenv(name) {
	case "":
		return def
	case "true":
		return true
	case "false":
		return false
	default:
		p.fail(name, "must be true or false")
		return def
	}
}

func (p *parser) oneOf(name, def string, allowed ...string) string {
	v := p.getenv(name)
	if v == "" {
		return def
	}
	if slices.Contains(allowed, v) {
		return v
	}
	p.fail(name, "must be one of "+strings.Join(allowed, ", "))
	return def
}

func (p *parser) logLevel(name string, def slog.Level) slog.Level {
	levels := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	v := p.getenv(name)
	if v == "" {
		return def
	}
	if l, ok := levels[v]; ok {
		return l
	}
	p.fail(name, "must be one of debug, info, warn, error")
	return def
}

// chainIDs reads chain IDs separated by commas; spaces around an item are ignored.
func (p *parser) chainIDs(name string, def []uint64) []uint64 {
	v := p.getenv(name)
	if v == "" {
		return def
	}
	var ids []uint64
	for item := range strings.SplitSeq(v, ",") {
		// The strconv error is not wrapped: it quotes the value.
		id, err := strconv.ParseUint(strings.TrimSpace(item), 10, 64)
		if err != nil || id == 0 {
			p.fail(name, "must be positive integer chain IDs separated by commas")
			return def
		}
		ids = append(ids, id)
	}
	return ids
}

func joinUint(ids []uint64) string {
	items := make([]string, len(ids))
	for i, id := range ids {
		items[i] = strconv.FormatUint(id, 10)
	}
	return strings.Join(items, ",")
}
