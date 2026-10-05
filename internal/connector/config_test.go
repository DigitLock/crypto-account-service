package connector

import (
	"encoding/json"
	"testing"
	"time"
)

// C1-T706 — Req: §3.1. The defaults and the parsing of sources.config; internal/grpc/api reads the flag.
func TestT706_StaleAfterDefault(t *testing.T) {
	for config, want := range map[string][3]time.Duration{
		`{}`: {15 * time.Minute, time.Hour, 30 * time.Minute},
		`{"sync_interval": {"balances": "10m", "ops": "20m"}}`:                      {10 * time.Minute, 20 * time.Minute, 20 * time.Minute},
		`{"sync_interval": {"balances": "5m"}, "stale_after": "30m"}`:               {5 * time.Minute, time.Hour, 30 * time.Minute},
		`{"sync_interval": {"balances": 900}, "stale_after": "soon"}`:               {15 * time.Minute, time.Hour, 30 * time.Minute},
		`{"sync_interval": {"balances": "-5m", "ops": "0s"}, "stale_after": "-1h"}`: {15 * time.Minute, time.Hour, 30 * time.Minute},
		`not json`: {15 * time.Minute, time.Hour, 30 * time.Minute},
		``:         {15 * time.Minute, time.Hour, 30 * time.Minute},
	} {
		src := Source{Config: json.RawMessage(config)}
		got := [3]time.Duration{SyncInterval(src, "balances"), SyncInterval(src, "ops"), StaleAfter(src)}
		if got != want {
			t.Errorf("%s: balances %v, ops %v, stale_after %v; want %v", config, got[0], got[1], got[2], want)
		}
	}
}
