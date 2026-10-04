// Package config loads the environment parameters of server (SRS — Core §3.1).
// Parameters stored in sources.config are not part of this package.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MasterKeySize is the length of CAS_MASTER_KEY after base64 decoding (ADR-4).
const MasterKeySize = 32

// Config is the parsed environment of server.
// CAS_MASTER_KEY and DATABASE_URL are held as Secret values: no method prints them,
// and LogValue omits them.
type Config struct {
	MasterKey        Secret[[]byte]
	MasterKeyVersion int
	DatabaseURL      Secret[string]

	DBPoolMaxConns int32
	DBPoolMinConns int32

	GRPCPort       int
	HealthHTTPPort int

	LogLevel  slog.Level
	LogFormat string

	ShutdownTimeout time.Duration

	SyncTick             time.Duration
	SyncWorkers          int
	SyncLockRetry        time.Duration
	SyncFailureThreshold int
	SyncBackoffInitial   time.Duration
	SyncBackoffMax       time.Duration
	TriggerSyncCooldown  time.Duration
	KeyCheckInterval     time.Duration

	EnableFakeSource bool
}

// Log formats of LOG_FORMAT.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Load reads and validates the environment through getenv. An empty value counts as unset.
// The returned error names every invalid variable and never contains a value.
func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	cfg := Config{
		MasterKey:        p.masterKey("CAS_MASTER_KEY"),
		MasterKeyVersion: p.integer("CAS_MASTER_KEY_VERSION", 1, 1, 1<<31-1),
		DatabaseURL:      Secret[string]{v: p.required("DATABASE_URL")},

		DBPoolMaxConns: int32(p.integer("DB_POOL_MAX_CONNS", 10, 1, 1<<31-1)),
		DBPoolMinConns: int32(p.integer("DB_POOL_MIN_CONNS", 2, 0, 1<<31-1)),

		GRPCPort:       p.integer("GRPC_PORT", 50053, 1, 65535),
		HealthHTTPPort: p.integer("HEALTH_HTTP_PORT", 8091, 1, 65535),

		LogLevel:  p.logLevel("LOG_LEVEL", slog.LevelInfo),
		LogFormat: p.oneOf("LOG_FORMAT", LogFormatJSON, LogFormatJSON, LogFormatText),

		ShutdownTimeout: p.duration("SHUTDOWN_TIMEOUT", 15*time.Second),

		SyncTick:             p.duration("SYNC_TICK", time.Second),
		SyncWorkers:          p.integer("SYNC_WORKERS", 4, 1, 1<<31-1),
		SyncLockRetry:        p.duration("SYNC_LOCK_RETRY", 10*time.Second),
		SyncFailureThreshold: p.integer("SYNC_FAILURE_THRESHOLD", 5, 1, 1<<31-1),
		SyncBackoffInitial:   p.duration("SYNC_BACKOFF_INITIAL", 30*time.Second),
		SyncBackoffMax:       p.duration("SYNC_BACKOFF_MAX", time.Hour),
		TriggerSyncCooldown:  p.duration("TRIGGER_SYNC_COOLDOWN", 60*time.Second),
		KeyCheckInterval:     p.duration("KEY_CHECK_INTERVAL", 24*time.Hour),

		EnableFakeSource: p.boolean("ENABLE_FAKE_SOURCE", false),
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

// LogValue implements slog.LogValuer. CAS_MASTER_KEY and DATABASE_URL are omitted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("CAS_MASTER_KEY_VERSION", c.MasterKeyVersion),
		slog.Int("DB_POOL_MAX_CONNS", int(c.DBPoolMaxConns)),
		slog.Int("DB_POOL_MIN_CONNS", int(c.DBPoolMinConns)),
		slog.Int("GRPC_PORT", c.GRPCPort),
		slog.Int("HEALTH_HTTP_PORT", c.HealthHTTPPort),
		slog.String("LOG_LEVEL", strings.ToLower(c.LogLevel.String())),
		slog.String("LOG_FORMAT", c.LogFormat),
		slog.Duration("SHUTDOWN_TIMEOUT", c.ShutdownTimeout),
		slog.Duration("SYNC_TICK", c.SyncTick),
		slog.Int("SYNC_WORKERS", c.SyncWorkers),
		slog.Duration("SYNC_LOCK_RETRY", c.SyncLockRetry),
		slog.Int("SYNC_FAILURE_THRESHOLD", c.SyncFailureThreshold),
		slog.Duration("SYNC_BACKOFF_INITIAL", c.SyncBackoffInitial),
		slog.Duration("SYNC_BACKOFF_MAX", c.SyncBackoffMax),
		slog.Duration("TRIGGER_SYNC_COOLDOWN", c.TriggerSyncCooldown),
		slog.Duration("KEY_CHECK_INTERVAL", c.KeyCheckInterval),
		slog.Bool("ENABLE_FAKE_SOURCE", c.EnableFakeSource),
	)
}

// Secret holds a value that must never be printed. Every fmt verb, String, GoString,
// LogValue and text marshalling yield "[redacted]"; Value returns the value itself.
type Secret[T []byte | string] struct {
	v T
}

const redacted = "[redacted]"

// Value returns the secret value.
func (s Secret[T]) Value() T { return s.v }

func (Secret[T]) String() string               { return redacted }
func (Secret[T]) GoString() string             { return redacted }
func (Secret[T]) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (Secret[T]) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret[T]) MarshalText() ([]byte, error) { return []byte(redacted), nil }

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

func (p *parser) masterKey(name string) Secret[[]byte] {
	v := p.required(name)
	if v == "" {
		return Secret[[]byte]{}
	}
	// The decoding error is not wrapped: it would point into the value.
	key, err := base64.StdEncoding.Strict().DecodeString(v)
	if err != nil || len(key) != MasterKeySize {
		p.fail(name, fmt.Sprintf("must be standard base64 of exactly %d bytes", MasterKeySize))
		return Secret[[]byte]{}
	}
	return Secret[[]byte]{v: key}
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
