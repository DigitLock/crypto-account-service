// Phase 1 of docs/test-plan-s2.md: the configuration of card-auth. An external test package, so the names of
// the rows do not collide with the rows of C1 in config_test.go.
package config_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/DigitLock/crypto-account-service/internal/config"
)

// Addresses of the tests: derived at run time, well-formed EIP-55, only parsed and never called.
var (
	controllerAddress = crypto.CreateAddress(common.BytesToAddress([]byte{1}), 0).Hex()
	tokenAddress      = crypto.CreateAddress(common.BytesToAddress([]byte{1}), 1).Hex()
)

// badChecksum flips the case of the first letter of address, which breaks its EIP-55 checksum.
func badChecksum(address string) string {
	b := []byte(address)
	for i := 2; i < len(b); i++ {
		switch {
		case b[i] >= 'a' && b[i] <= 'f':
			b[i] -= 'a' - 'A'
			return string(b)
		case b[i] >= 'A' && b[i] <= 'F':
			b[i] += 'a' - 'A'
			return string(b)
		}
	}
	panic("address without a letter")
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// cardAuthEnv returns the required variables; the key and the URLs are generated at run time.
func cardAuthEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"CARD_AUTH_DATABASE_URL":       "postgres://cas_card_auth:" + randomHex(t, 8) + "@localhost/cas",
		"OPERATOR_PRIVATE_KEY":         randomHex(t, 32),
		"CARD_AUTH_CHAIN_ID":           "31337",
		"CARD_AUTH_RPC_URL":            "http://127.0.0.1:8545/" + randomHex(t, 8),
		"CARD_AUTH_CONTROLLER_ADDRESS": controllerAddress,
		"CARD_AUTH_TOKEN_ADDRESS":      tokenAddress,
		"CARD_AUTH_TOKEN_DECIMALS":     "6",
	}
}

var requiredCardAuth = []string{
	"CARD_AUTH_DATABASE_URL", "OPERATOR_PRIVATE_KEY", "CARD_AUTH_CHAIN_ID", "CARD_AUTH_RPC_URL",
	"CARD_AUTH_CONTROLLER_ADDRESS", "CARD_AUTH_TOKEN_ADDRESS", "CARD_AUTH_TOKEN_DECIMALS",
}

func getenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// secretValues returns the secret values of env, as given and as parsed.
func secretValues(env map[string]string) []string {
	var out []string
	for _, name := range []string{"CARD_AUTH_DATABASE_URL", "OPERATOR_PRIVATE_KEY", "CARD_AUTH_RPC_URL",
		"CARD_AUTH_RPC_FALLBACK_URL", "CARD_AUTH_RPC_WS_URL"} {
		if v := env[name]; v != "" {
			out = append(out, v, strings.TrimPrefix(v, "0x"))
		}
	}
	return out
}

func assertNone(t *testing.T, s string, values ...string) {
	t.Helper()
	for _, v := range values {
		if v != "" && strings.Contains(s, v) {
			t.Errorf("output contains a secret value: %q", s)
		}
	}
}

// S2-T101 — Req: SRS — Card Spend §3.1
func TestT101_RequiredConfiguration(t *testing.T) {
	for _, name := range requiredCardAuth {
		t.Run(name, func(t *testing.T) {
			env := cardAuthEnv(t)
			secrets := secretValues(env)
			delete(env, name)

			_, err := config.LoadCardAuth(getenv(env))
			if err == nil {
				t.Fatal("LoadCardAuth succeeded without " + name)
			}
			if !strings.Contains(err.Error(), name+" is required") {
				t.Errorf("error %q does not name %s", err, name)
			}
			assertNone(t, err.Error(), secrets...)
		})
	}

	t.Run("all missing", func(t *testing.T) {
		_, err := config.LoadCardAuth(getenv(nil))
		if err == nil {
			t.Fatal("LoadCardAuth succeeded on an empty environment")
		}
		for _, name := range requiredCardAuth {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name %s", name)
			}
		}
	})

	t.Run("secrets are never printed", func(t *testing.T) {
		env := cardAuthEnv(t)
		env["OPERATOR_PRIVATE_KEY"] = "0x" + env["OPERATOR_PRIVATE_KEY"]
		env["CARD_AUTH_RPC_FALLBACK_URL"] = "https://fallback.invalid/" + randomHex(t, 8)
		env["CARD_AUTH_RPC_WS_URL"] = "wss://ws.invalid/" + randomHex(t, 8)
		cfg, err := config.LoadCardAuth(getenv(env))
		if err != nil {
			t.Fatal(err)
		}
		key := cfg.OperatorKey.Value()
		secrets := append(secretValues(env), string(key), strings.ToUpper(hex.EncodeToString(key)))

		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"} {
			assertNone(t, fmt.Sprintf(verb, cfg), secrets...)
		}
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("start", "config", cfg)
		slog.New(slog.NewTextHandler(&buf, nil)).Info("start", "config", cfg)
		assertNone(t, buf.String(), secrets...)
		for _, name := range []string{"OPERATOR_PRIVATE_KEY", "CARD_AUTH_DATABASE_URL", "CARD_AUTH_RPC_URL",
			"CARD_AUTH_RPC_FALLBACK_URL", "CARD_AUTH_RPC_WS_URL"} {
			if strings.Contains(buf.String(), name) {
				t.Errorf("log names %s: %s", name, buf.String())
			}
		}
		for _, want := range []string{`"CARD_AUTH_DECISION_DEADLINE":"2.5s"`, `"CARD_AUTH_CHAIN_ID":31337`,
			`"CARD_AUTH_CONTROLLER_ADDRESS":"` + controllerAddress + `"`, `"CARD_AUTH_DEBIT_GAS_LIMIT":176000`} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("log does not show %s: %s", want, buf.String())
			}
		}
		js, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertNone(t, string(js), secrets...)
	})
}

// S2-T102 — Req: SRS — Card Spend §3.1
func TestT102_Defaults(t *testing.T) {
	env := cardAuthEnv(t)
	cfg, err := config.LoadCardAuth(getenv(env))
	if err != nil {
		t.Fatal(err)
	}

	if hex.EncodeToString(cfg.OperatorKey.Value()) != env["OPERATOR_PRIVATE_KEY"] {
		t.Error("operator key is not the decoded OPERATOR_PRIVATE_KEY")
	}
	if cfg.DatabaseURL.Value() != env["CARD_AUTH_DATABASE_URL"] || cfg.RPCURL.Value() != env["CARD_AUTH_RPC_URL"] {
		t.Error("the URLs are not the given ones")
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"CARD_AUTH_CHAIN_ID", cfg.ChainID, uint64(31337)},
		{"CARD_AUTH_CONTROLLER_ADDRESS", cfg.ControllerAddress, common.HexToAddress(controllerAddress)},
		{"CARD_AUTH_TOKEN_ADDRESS", cfg.TokenAddress, common.HexToAddress(tokenAddress)},
		{"CARD_AUTH_TOKEN_DECIMALS", cfg.TokenDecimals, uint8(6)},
		{"CARD_AUTH_RPC_FALLBACK_URL", cfg.RPCFallbackURL.Value(), ""},
		{"CARD_AUTH_RPC_WS_URL", cfg.RPCWSURL.Value(), ""},
		{"CARD_AUTH_DECISION_DEADLINE", cfg.DecisionDeadline, 2500 * time.Millisecond},
		{"CARD_AUTH_DEBIT_VALIDITY", cfg.DebitValidity, 4 * time.Second},
		{"CARD_AUTH_MIN_SEND_WINDOW", cfg.MinSendWindow, 500 * time.Millisecond},
		{"CARD_AUTH_RPC_READ_TIMEOUT", cfg.RPCReadTimeout, 500 * time.Millisecond},
		{"CARD_AUTH_RECEIPT_POLL_INTERVAL", cfg.ReceiptPollInterval, 200 * time.Millisecond},
		{"CARD_AUTH_QUOTE_BUFFER_BPS", cfg.QuoteBufferBPS, 100},
		{"CARD_AUTH_FINALITY_MODE", cfg.FinalityMode, "confirmations"},
		{"CARD_AUTH_FINALITY_TAG", cfg.FinalityTag, "finalized"},
		{"CARD_AUTH_FINALITY_CONFIRMATIONS", cfg.FinalityConfirmations, 10},
		{"CARD_AUTH_TRACKER_INTERVAL", cfg.TrackerInterval, 2 * time.Second},
		{"CARD_AUTH_RETURN_RETRY_INTERVAL", cfg.ReturnRetryInterval, 30 * time.Second},
		{"CARD_AUTH_RPC_FALLBACK_AFTER", cfg.RPCFallbackAfter, 3},
		{"CARD_AUTH_FEE_BUMP_PERCENT", cfg.FeeBumpPercent, 25},
		{"CARD_AUTH_LISTENER_SUBSCRIPTION", cfg.ListenerSubscription, "pendingLogs"},
		{"CARD_AUTH_DEBIT_GAS_LIMIT", cfg.DebitGasLimit, uint64(176000)},
		{"CARD_AUTH_REFUND_GAS_LIMIT", cfg.RefundGasLimit, uint64(145000)},
		{"CRS_ADDRESS", cfg.CRSAddress, ""},
		{"CARD_AUTH_HTTP_PORT", cfg.HTTPPort, 8092},
		{"CARD_AUTH_HEALTH_PORT", cfg.HealthPort, 8093},
		{"CARD_AUTH_DB_POOL_MAX_CONNS", cfg.DBPoolMaxConns, int32(10)},
		{"CARD_AUTH_DB_POOL_MIN_CONNS", cfg.DBPoolMinConns, int32(2)},
		{"CARD_AUTH_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout, 15 * time.Second},
		{"EVM_ALLOWED_CHAIN_IDS", fmt.Sprint(cfg.EVMAllowedChainIDs), "[31337 84532]"},
		{"LOG_LEVEL", cfg.LogLevel, slog.LevelInfo},
		{"LOG_FORMAT", cfg.LogFormat, "json"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	t.Run("set values override defaults", func(t *testing.T) {
		env := cardAuthEnv(t)
		set := map[string]string{
			"OPERATOR_PRIVATE_KEY":            "0x" + strings.ToUpper(env["OPERATOR_PRIVATE_KEY"]),
			"CARD_AUTH_CHAIN_ID":              "84532",
			"CARD_AUTH_CONTROLLER_ADDRESS":    strings.ToLower(controllerAddress),
			"CARD_AUTH_TOKEN_DECIMALS":        "18",
			"CARD_AUTH_RPC_FALLBACK_URL":      "https://fallback.invalid/v1",
			"CARD_AUTH_RPC_WS_URL":            "ws://127.0.0.1:8546",
			"CARD_AUTH_DECISION_DEADLINE":     "1s",
			"CARD_AUTH_DEBIT_VALIDITY":        "2s",
			"CARD_AUTH_FINALITY_MODE":         "tag",
			"CARD_AUTH_FINALITY_TAG":          "safe",
			"CARD_AUTH_FEE_BUMP_PERCENT":      "10",
			"CARD_AUTH_LISTENER_SUBSCRIPTION": "logs",
			"CARD_AUTH_DEBIT_GAS_LIMIT":       "21001",
			"CARD_AUTH_REFUND_GAS_LIMIT":      "150000",
			"CARD_AUTH_QUOTE_BUFFER_BPS":      "0",
			"CRS_ADDRESS":                     "localhost:50051",
			"CARD_AUTH_HTTP_PORT":             "9092",
			"LOG_FORMAT":                      "text",
		}
		for k, v := range set {
			env[k] = v
		}
		cfg, err := config.LoadCardAuth(getenv(env))
		if err != nil {
			t.Fatal(err)
		}
		if strings.ToUpper(hex.EncodeToString(cfg.OperatorKey.Value())) != strings.TrimPrefix(set["OPERATOR_PRIVATE_KEY"], "0x") {
			t.Error("the key with 0x and upper case is not decoded")
		}
		if cfg.ChainID != 84532 || cfg.ControllerAddress != common.HexToAddress(controllerAddress) ||
			cfg.TokenDecimals != 18 || cfg.RPCFallbackURL.Value() != set["CARD_AUTH_RPC_FALLBACK_URL"] ||
			cfg.RPCWSURL.Value() != set["CARD_AUTH_RPC_WS_URL"] || cfg.DebitValidity != 2*time.Second ||
			cfg.FinalityMode != "tag" || cfg.FinalityTag != "safe" || cfg.FeeBumpPercent != 10 ||
			cfg.ListenerSubscription != "logs" || cfg.DebitGasLimit != 21001 || cfg.RefundGasLimit != 150000 ||
			cfg.QuoteBufferBPS != 0 || cfg.CRSAddress != "localhost:50051" || cfg.HTTPPort != 9092 || cfg.LogFormat != "text" {
			t.Errorf("set values not applied: %+v", cfg)
		}
	})
}

// S2-T103 — Req: SRS — Card Spend §3.1
func TestT103_InvalidConfiguration(t *testing.T) {
	cases := []struct {
		name     string
		set      map[string]string
		variable string
	}{
		{"debit validity below deadline + 1s", map[string]string{"CARD_AUTH_DEBIT_VALIDITY": "3499ms"}, "CARD_AUTH_DEBIT_VALIDITY"},
		{"debit validity equal to deadline", map[string]string{"CARD_AUTH_DECISION_DEADLINE": "4s"}, "CARD_AUTH_DEBIT_VALIDITY"},
		{"min send window equal to deadline", map[string]string{"CARD_AUTH_MIN_SEND_WINDOW": "2.5s"}, "CARD_AUTH_MIN_SEND_WINDOW"},
		{"min send window above deadline", map[string]string{"CARD_AUTH_MIN_SEND_WINDOW": "3s"}, "CARD_AUTH_MIN_SEND_WINDOW"},
		{"chain ID outside the allow-list", map[string]string{"CARD_AUTH_CHAIN_ID": "1"}, "CARD_AUTH_CHAIN_ID"},
		{"chain ID outside a set allow-list", map[string]string{"EVM_ALLOWED_CHAIN_IDS": "84532"}, "CARD_AUTH_CHAIN_ID"},
		{"chain ID not a number", map[string]string{"CARD_AUTH_CHAIN_ID": "anvil"}, "CARD_AUTH_CHAIN_ID"},
		{"chain ID zero", map[string]string{"CARD_AUTH_CHAIN_ID": "0"}, "CARD_AUTH_CHAIN_ID"},
		{"allow-list malformed", map[string]string{"EVM_ALLOWED_CHAIN_IDS": "31337,,84532"}, "EVM_ALLOWED_CHAIN_IDS"},
		{"key of 31 bytes", map[string]string{"OPERATOR_PRIVATE_KEY": strings.Repeat("ab", 31)}, "OPERATOR_PRIVATE_KEY"},
		{"key of 33 bytes", map[string]string{"OPERATOR_PRIVATE_KEY": strings.Repeat("ab", 33)}, "OPERATOR_PRIVATE_KEY"},
		{"key not hexadecimal", map[string]string{"OPERATOR_PRIVATE_KEY": strings.Repeat("zz", 32)}, "OPERATOR_PRIVATE_KEY"},
		{"key with 0X prefix", map[string]string{"OPERATOR_PRIVATE_KEY": "0X" + strings.Repeat("ab", 32)}, "OPERATOR_PRIVATE_KEY"},
		{"key with spaces", map[string]string{"OPERATOR_PRIVATE_KEY": " " + strings.Repeat("ab", 32)}, "OPERATOR_PRIVATE_KEY"},
		{"finality mode unknown", map[string]string{"CARD_AUTH_FINALITY_MODE": "blocks"}, "CARD_AUTH_FINALITY_MODE"},
		{"finality tag unknown", map[string]string{"CARD_AUTH_FINALITY_TAG": "latest"}, "CARD_AUTH_FINALITY_TAG"},
		{"finality confirmations zero", map[string]string{"CARD_AUTH_FINALITY_CONFIRMATIONS": "0"}, "CARD_AUTH_FINALITY_CONFIRMATIONS"},
		{"malformed duration", map[string]string{"CARD_AUTH_TRACKER_INTERVAL": "2 seconds"}, "CARD_AUTH_TRACKER_INTERVAL"},
		{"duration without unit", map[string]string{"CARD_AUTH_RPC_READ_TIMEOUT": "500"}, "CARD_AUTH_RPC_READ_TIMEOUT"},
		{"zero duration", map[string]string{"CARD_AUTH_SHUTDOWN_TIMEOUT": "0s"}, "CARD_AUTH_SHUTDOWN_TIMEOUT"},
		{"negative duration", map[string]string{"CARD_AUTH_RETURN_RETRY_INTERVAL": "-30s"}, "CARD_AUTH_RETURN_RETRY_INTERVAL"},
		{"decimals above 18", map[string]string{"CARD_AUTH_TOKEN_DECIMALS": "19"}, "CARD_AUTH_TOKEN_DECIMALS"},
		{"decimals negative", map[string]string{"CARD_AUTH_TOKEN_DECIMALS": "-1"}, "CARD_AUTH_TOKEN_DECIMALS"},
		{"decimals not a number", map[string]string{"CARD_AUTH_TOKEN_DECIMALS": "six"}, "CARD_AUTH_TOKEN_DECIMALS"},
		{"controller zero address", map[string]string{"CARD_AUTH_CONTROLLER_ADDRESS": "0x" + strings.Repeat("0", 40)}, "CARD_AUTH_CONTROLLER_ADDRESS"},
		{"controller without 0x", map[string]string{"CARD_AUTH_CONTROLLER_ADDRESS": controllerAddress[2:]}, "CARD_AUTH_CONTROLLER_ADDRESS"},
		{"controller too short", map[string]string{"CARD_AUTH_CONTROLLER_ADDRESS": controllerAddress[:41]}, "CARD_AUTH_CONTROLLER_ADDRESS"},
		{"token bad EIP-55 checksum", map[string]string{"CARD_AUTH_TOKEN_ADDRESS": badChecksum(tokenAddress)}, "CARD_AUTH_TOKEN_ADDRESS"},
		{"token not hexadecimal", map[string]string{"CARD_AUTH_TOKEN_ADDRESS": "0x" + strings.Repeat("g", 40)}, "CARD_AUTH_TOKEN_ADDRESS"},
		{"RPC URL without scheme", map[string]string{"CARD_AUTH_RPC_URL": "127.0.0.1:8545"}, "CARD_AUTH_RPC_URL"},
		{"RPC URL WebSocket", map[string]string{"CARD_AUTH_RPC_URL": "ws://127.0.0.1:8545"}, "CARD_AUTH_RPC_URL"},
		{"fallback URL without host", map[string]string{"CARD_AUTH_RPC_FALLBACK_URL": "https:///v1"}, "CARD_AUTH_RPC_FALLBACK_URL"},
		{"WebSocket URL HTTP", map[string]string{"CARD_AUTH_RPC_WS_URL": "https://ws.invalid"}, "CARD_AUTH_RPC_WS_URL"},
		{"fee bump below 10", map[string]string{"CARD_AUTH_FEE_BUMP_PERCENT": "9"}, "CARD_AUTH_FEE_BUMP_PERCENT"},
		{"listener subscription unknown", map[string]string{"CARD_AUTH_LISTENER_SUBSCRIPTION": "newHeads"}, "CARD_AUTH_LISTENER_SUBSCRIPTION"},
		{"debit gas limit 21000", map[string]string{"CARD_AUTH_DEBIT_GAS_LIMIT": "21000"}, "CARD_AUTH_DEBIT_GAS_LIMIT"},
		{"refund gas limit not a number", map[string]string{"CARD_AUTH_REFUND_GAS_LIMIT": "lots"}, "CARD_AUTH_REFUND_GAS_LIMIT"},
		{"quote buffer negative", map[string]string{"CARD_AUTH_QUOTE_BUFFER_BPS": "-1"}, "CARD_AUTH_QUOTE_BUFFER_BPS"},
		{"fallback after zero", map[string]string{"CARD_AUTH_RPC_FALLBACK_AFTER": "0"}, "CARD_AUTH_RPC_FALLBACK_AFTER"},
		{"HTTP port out of range", map[string]string{"CARD_AUTH_HTTP_PORT": "70000"}, "CARD_AUTH_HTTP_PORT"},
		{"health port not a number", map[string]string{"CARD_AUTH_HEALTH_PORT": "health"}, "CARD_AUTH_HEALTH_PORT"},
		{"pool min above max", map[string]string{"CARD_AUTH_DB_POOL_MIN_CONNS": "20"}, "CARD_AUTH_DB_POOL_MIN_CONNS"},
		{"unknown log level", map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL"},
		{"unknown log format", map[string]string{"LOG_FORMAT": "yaml-ish"}, "LOG_FORMAT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := cardAuthEnv(t)
			for k, v := range c.set {
				env[k] = v
			}
			_, err := config.LoadCardAuth(getenv(env))
			if err == nil {
				t.Fatalf("LoadCardAuth accepted %s", c.variable)
			}
			if !strings.Contains(err.Error(), c.variable) {
				t.Errorf("error %q does not name %s", err, c.variable)
			}
			values := secretValues(env)
			for _, v := range c.set {
				if len(v) >= 3 {
					values = append(values, v)
				}
			}
			assertNone(t, err.Error(), values...)
		})
	}

	t.Run("every invalid variable is named", func(t *testing.T) {
		env := cardAuthEnv(t)
		env["CARD_AUTH_CHAIN_ID"] = "x"
		env["CARD_AUTH_FINALITY_MODE"] = "x"
		env["CARD_AUTH_TOKEN_ADDRESS"] = "x"
		_, err := config.LoadCardAuth(getenv(env))
		if err == nil {
			t.Fatal("LoadCardAuth accepted three invalid variables")
		}
		for _, name := range []string{"CARD_AUTH_CHAIN_ID", "CARD_AUTH_FINALITY_MODE", "CARD_AUTH_TOKEN_ADDRESS"} {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s", err, name)
			}
		}
	})
}
