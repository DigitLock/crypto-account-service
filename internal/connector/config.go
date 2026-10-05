package connector

import (
	"encoding/json"
	"time"
)

// Defaults of the values of sources.config (SRS — Core §3.1).
const (
	DefaultBalanceInterval = 15 * time.Minute
	DefaultLedgerInterval  = time.Hour
	familyBalances         = "balances"
)

// sourceConfig is the part of sources.config that connectors and the read API share. Durations are JSON
// strings such as "15m": {"sync_interval": {"balances": "15m", "ops": "1h"}, "stale_after": "30m"}.
type sourceConfig struct {
	SyncInterval map[string]json.RawMessage `json:"sync_interval"`
	StaleAfter   json.RawMessage            `json:"stale_after"`
}

func configOf(src Source) sourceConfig {
	var c sourceConfig
	_ = json.Unmarshal(src.Config, &c) // a malformed config gives every value its default
	return c
}

// SyncInterval is the sync interval of a stream family of a source: 15 min for balances and 1 h for any
// other family, unless sources.config sets a valid positive duration.
func SyncInterval(src Source, family string) time.Duration {
	def := DefaultLedgerInterval
	if family == familyBalances {
		def = DefaultBalanceInterval
	}
	return durationOr(configOf(src).SyncInterval[family], def)
}

// StaleAfter is the age of the last successful balance sync after which balances are stale: twice the
// balance interval, unless sources.config sets a valid positive duration.
func StaleAfter(src Source) time.Duration {
	return durationOr(configOf(src).StaleAfter, 2*SyncInterval(src, familyBalances))
}

func durationOr(raw json.RawMessage, def time.Duration) time.Duration {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
