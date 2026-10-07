package evm

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

func source(config string) connector.Source {
	return connector.Source{Code: "anvil", Kind: connector.KindEVM, Enabled: true, Config: json.RawMessage(config)}
}

// S3-T104 — Req: §3.1; S3 D-12. Defaults, validation and the error of a network without finality_mode.
func TestT104_ConfigDefaultsAndValidation(t *testing.T) {
	defaults := Config{
		ChainID: 31337, FinalityMode: FinalityModeConfirmations, FinalityTag: FinalityTagFinalized,
		FinalityConfirmations: 10, LogRangeMax: 2000, RPCRateLimit: 5, CompletenessInterval: time.Hour,
		BalancesInterval: 15 * time.Minute, LogsInterval: 5 * time.Minute,
	}

	t.Run("defaults", func(t *testing.T) {
		got, err := ParseConfig(source(`{"chain_id": 31337, "finality_mode": "confirmations"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got != defaults {
			t.Errorf("config = %+v, want %+v", got, defaults)
		}
	})

	t.Run("values of base-sepolia", func(t *testing.T) {
		got, err := ParseConfig(source(`{"chain_id": 84532, "finality_mode": "tag", "finality_tag": "finalized",
			"controller_address": "0xF75D58dc6E33487dB994D81D0d870D61Eac45F37", "backfill_floor": 47768907,
			"treasury_connection": "0b0b0b0b-0000-4000-8000-000000000001"}`))
		if err != nil {
			t.Fatal(err)
		}
		want := defaults
		want.ChainID, want.FinalityMode = 84532, FinalityModeTag
		want.ControllerAddress = "0xF75D58dc6E33487dB994D81D0d870D61Eac45F37"
		want.BackfillFloor, want.HasBackfillFloor = 47768907, true
		want.TreasuryConnection = "0b0b0b0b-0000-4000-8000-000000000001"
		if got != want {
			t.Errorf("config = %+v, want %+v", got, want)
		}
	})

	t.Run("set values", func(t *testing.T) {
		got, err := ParseConfig(source(`{"chain_id": 31337, "finality_mode": "tag", "finality_tag": "safe",
			"finality_confirmations": 3, "controller_address": "0xf75d58dc6e33487db994d81d0d870d61eac45f37",
			"backfill_floor": 0, "log_range_max": 500, "rpc_rate_limit": 25, "completeness_interval": "10m",
			"sync_interval": {"balances": "1m", "logs": "30s"}}`))
		if err != nil {
			t.Fatal(err)
		}
		want := Config{
			ChainID: 31337, FinalityMode: FinalityModeTag, FinalityTag: FinalityTagSafe, FinalityConfirmations: 3,
			ControllerAddress: "0xF75D58dc6E33487dB994D81D0d870D61Eac45F37", HasBackfillFloor: true,
			LogRangeMax: 500, RPCRateLimit: 25, CompletenessInterval: 10 * time.Minute,
			BalancesInterval: time.Minute, LogsInterval: 30 * time.Second,
		}
		if got != want {
			t.Errorf("config = %+v, want %+v", got, want)
		}
	})

	t.Run("malformed optional values take their default", func(t *testing.T) {
		for _, kv := range []string{
			`"finality_tag": "latest"`, `"finality_tag": 1`,
			`"finality_confirmations": 0`, `"finality_confirmations": -1`, `"finality_confirmations": "10"`,
			`"finality_confirmations": 2.5`, `"finality_confirmations": 1e1`,
			`"controller_address": "0x0000000000000000000000000000000000000000"`,
			`"controller_address": "0xF75D58dc6E33487dB994D81D0d870D61Eac45f37"`, // wrong checksum
			`"controller_address": "F75D58dc6E33487dB994D81D0d870D61Eac45F37"`,
			`"treasury_connection": "not-a-uuid"`, `"treasury_connection": "0b0b0b0b00004000800000000000000001"`,
			`"treasury_connection": 7`,
			`"backfill_floor": -1`, `"backfill_floor": "47768907"`, `"backfill_floor": 1.5`,
			`"log_range_max": 0`, `"log_range_max": "2000"`,
			`"rpc_rate_limit": 0`, `"rpc_rate_limit": 2.5`, `"rpc_rate_limit": "5"`, `"rpc_rate_limit": 99999999999`,
			`"completeness_interval": "soon"`, `"completeness_interval": "-1h"`, `"completeness_interval": 3600`,
			`"sync_interval": {"balances": "0s", "logs": "-5m"}`, `"sync_interval": {"logs": 300}`,
			`"sync_interval": "5m"`,
		} {
			got, err := ParseConfig(source(`{"chain_id": 31337, "finality_mode": "confirmations", ` + kv + `}`))
			if err != nil {
				t.Errorf("%s: %v", kv, err)
				continue
			}
			if got != defaults {
				t.Errorf("%s: config = %+v, want the defaults %+v", kv, got, defaults)
			}
		}
	})

	t.Run("network errors", func(t *testing.T) {
		for _, config := range []string{
			`{"chain_id": 31337}`,
			`{"chain_id": 31337, "finality_mode": "finalized"}`,
			`{"chain_id": 31337, "finality_mode": ""}`,
			`{"chain_id": 31337, "finality_mode": 1}`,
			`{"finality_mode": "tag"}`,
			`{"chain_id": "31337", "finality_mode": "tag"}`,
			`{"chain_id": 0, "finality_mode": "tag"}`,
			`not json`,
			``,
		} {
			_, err := ParseConfig(source(config))
			if !errors.Is(err, ErrNetworkConfig) {
				t.Errorf("%s: error %v, want ErrNetworkConfig", config, err)
				continue
			}
			if !strings.Contains(err.Error(), "chain_id") && !strings.Contains(err.Error(), "finality_mode") &&
				!strings.Contains(err.Error(), "JSON") {
				t.Errorf("%s: error %q names no key", config, err)
			}
		}
	})
}
