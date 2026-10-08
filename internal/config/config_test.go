package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// testEnv returns the required variables with values generated at run time.
func testEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"CAS_MASTER_KEY": base64.StdEncoding.EncodeToString(randomBytes(t, MasterKeySize)),
		"DATABASE_URL":   "database-url-" + hex.EncodeToString(randomBytes(t, 8)),
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func getenvFrom(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// assertNoValues fails when s contains any of the values.
func assertNoValues(t *testing.T, s string, values ...string) {
	t.Helper()
	for _, v := range values {
		if v != "" && strings.Contains(s, v) {
			t.Errorf("output contains a configuration value: %q", s)
		}
	}
}

// C1-T101 — Req: §3.1
func TestT101_RequiredConfiguration(t *testing.T) {
	for _, missing := range [][]string{
		{"DATABASE_URL"},
		{"CAS_MASTER_KEY"},
		{"DATABASE_URL", "CAS_MASTER_KEY"},
	} {
		t.Run(strings.Join(missing, "+"), func(t *testing.T) {
			env := testEnv(t)
			present := []string{env["CAS_MASTER_KEY"], env["DATABASE_URL"]}
			for _, name := range missing {
				delete(env, name)
			}

			_, err := Load(getenvFrom(env))
			if err == nil {
				t.Fatal("Load succeeded without a required variable")
			}
			for _, name := range missing {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %s", err, name)
				}
			}
			assertNoValues(t, err.Error(), present...)
		})
	}

	t.Run("secrets are never printed", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := Load(getenvFrom(env))
		if err != nil {
			t.Fatal(err)
		}
		rawKey := cfg.MasterKey.Value()
		secrets := []string{env["CAS_MASTER_KEY"], env["DATABASE_URL"], hex.EncodeToString(rawKey), string(rawKey)}

		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"} {
			assertNoValues(t, fmt.Sprintf(verb, cfg), secrets...)
		}

		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("start", "config", cfg)
		slog.New(slog.NewTextHandler(&buf, nil)).Info("start", "config", cfg)
		assertNoValues(t, buf.String(), secrets...)
		for _, name := range []string{"CAS_MASTER_KEY\"", "CAS_MASTER_KEY=", "DATABASE_URL"} {
			if strings.Contains(buf.String(), name) {
				t.Errorf("log names %s: %s", name, buf.String())
			}
		}

		for _, want := range []string{`"SHUTDOWN_TIMEOUT":"15s"`, `"EVM_ALLOWED_CHAIN_IDS":"31337,84532"`, `"SYNC_MAX_PAGES_PER_RUN":20`, `"KEY_CHECK_INTERVAL":"24h0m0s"`, `SYNC_TICK=1s`} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("log does not show the duration as %s: %s", want, buf.String())
			}
		}

		js, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertNoValues(t, string(js), secrets...)
	})
}

// C1-T102 — Req: §3.1
func TestT102_Defaults(t *testing.T) {
	env := testEnv(t)
	cfg, err := Load(getenvFrom(env))
	if err != nil {
		t.Fatal(err)
	}

	if got := base64.StdEncoding.EncodeToString(cfg.MasterKey.Value()); got != env["CAS_MASTER_KEY"] {
		t.Error("master key is not the decoded CAS_MASTER_KEY")
	}
	if cfg.DatabaseURL.Value() != env["DATABASE_URL"] {
		t.Error("DatabaseURL is not DATABASE_URL")
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"CAS_MASTER_KEY_VERSION", cfg.MasterKeyVersion, 1},
		{"DB_POOL_MAX_CONNS", cfg.DBPoolMaxConns, int32(10)},
		{"DB_POOL_MIN_CONNS", cfg.DBPoolMinConns, int32(2)},
		{"GRPC_PORT", cfg.GRPCPort, 50053},
		{"HEALTH_HTTP_PORT", cfg.HealthHTTPPort, 8091},
		{"LOG_LEVEL", cfg.LogLevel, slog.LevelInfo},
		{"LOG_FORMAT", cfg.LogFormat, "json"},
		{"SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout, 15 * time.Second},
		{"SYNC_TICK", cfg.SyncTick, time.Second},
		{"SYNC_WORKERS", cfg.SyncWorkers, 4},
		{"SYNC_MAX_PAGES_PER_RUN", cfg.SyncMaxPagesPerRun, 20},
		{"SYNC_LOCK_RETRY", cfg.SyncLockRetry, 10 * time.Second},
		{"SYNC_FAILURE_THRESHOLD", cfg.SyncFailureThreshold, 5},
		{"SYNC_BACKOFF_INITIAL", cfg.SyncBackoffInitial, 30 * time.Second},
		{"SYNC_BACKOFF_MAX", cfg.SyncBackoffMax, time.Hour},
		{"TRIGGER_SYNC_COOLDOWN", cfg.TriggerSyncCooldown, 60 * time.Second},
		{"KEY_CHECK_INTERVAL", cfg.KeyCheckInterval, 24 * time.Hour},
		{"ENABLE_FAKE_SOURCE", cfg.EnableFakeSource, false},
		{"EVM_ALLOWED_CHAIN_IDS", fmt.Sprint(cfg.EVMAllowedChainIDs), "[31337 84532]"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	t.Run("set values override defaults", func(t *testing.T) {
		env := testEnv(t)
		env["GRPC_PORT"] = "6000"
		env["SYNC_BACKOFF_MAX"] = "2h"
		env["LOG_FORMAT"] = "text"
		env["LOG_LEVEL"] = "debug"
		env["ENABLE_FAKE_SOURCE"] = "true"
		env["CAS_MASTER_KEY_VERSION"] = "32767"
		env["EVM_ALLOWED_CHAIN_IDS"] = " 31337 ,84532,  11155111 "
		cfg, err := Load(getenvFrom(env))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GRPCPort != 6000 || cfg.SyncBackoffMax != 2*time.Hour || cfg.LogFormat != "text" ||
			cfg.LogLevel != slog.LevelDebug || !cfg.EnableFakeSource || cfg.MasterKeyVersion != 32767 ||
			fmt.Sprint(cfg.EVMAllowedChainIDs) != "[31337 84532 11155111]" {
			t.Errorf("set values not applied: %+v", cfg)
		}
	})
}

// C1-T103 — Req: §3.1, ADR-4
func TestT103_InvalidConfiguration(t *testing.T) {
	// 32 bytes whose standard base64 contains '+' and '/', so the URL-safe form differs.
	urlSafeKey := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff}, MasterKeySize/2))

	cases := []struct {
		name, variable, value string
	}{
		{"non-numeric gRPC port", "GRPC_PORT", "port-abc"},
		{"non-numeric health port", "HEALTH_HTTP_PORT", "80a"},
		{"port out of range", "GRPC_PORT", "70000"},
		{"master key not base64", "CAS_MASTER_KEY", "not*base64*at*all"},
		{"master key of 31 bytes", "CAS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 31))},
		{"master key of 33 bytes", "CAS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 33))},
		{"master key URL-safe base64", "CAS_MASTER_KEY", urlSafeKey},
		{"master key unpadded", "CAS_MASTER_KEY", base64.RawStdEncoding.EncodeToString(make([]byte, 32))},
		{"duration without unit", "SYNC_TICK", "10"},
		{"malformed duration", "SHUTDOWN_TIMEOUT", "15 seconds"},
		{"negative duration", "SYNC_BACKOFF_INITIAL", "-30s"},
		{"zero duration", "KEY_CHECK_INTERVAL", "0s"},
		{"unknown log format", "LOG_FORMAT", "yaml-ish"},
		{"unknown log level", "LOG_LEVEL", "verbose"},
		{"boolean not true or false", "ENABLE_FAKE_SOURCE", "yes-please"},
		{"zero workers", "SYNC_WORKERS", "0"},
		{"zero pages per run", "SYNC_MAX_PAGES_PER_RUN", "0"},
		{"pages per run not a number", "SYNC_MAX_PAGES_PER_RUN", "many"},
		{"master key version zero", "CAS_MASTER_KEY_VERSION", "0"},
		{"master key version above SMALLINT", "CAS_MASTER_KEY_VERSION", "32768"},
		{"chain ID not a number", "EVM_ALLOWED_CHAIN_IDS", "31337,anvil"},
		{"chain ID zero", "EVM_ALLOWED_CHAIN_IDS", "0"},
		{"chain ID negative", "EVM_ALLOWED_CHAIN_IDS", "31337,-5"},
		{"empty chain ID item", "EVM_ALLOWED_CHAIN_IDS", "31337,,84532"},
		{"pool min above max", "DB_POOL_MIN_CONNS", "20"},
		{"backoff initial above max", "SYNC_BACKOFF_INITIAL", "2h"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := testEnv(t)
			env[c.variable] = c.value

			_, err := Load(getenvFrom(env))
			if err == nil {
				t.Fatalf("Load accepted %s", c.variable)
			}
			if !strings.Contains(err.Error(), c.variable) {
				t.Errorf("error %q does not name %s", err, c.variable)
			}
			values := []string{env["CAS_MASTER_KEY"], env["DATABASE_URL"]}
			if len(c.value) >= 3 {
				values = append(values, c.value)
			}
			assertNoValues(t, err.Error(), values...)
		})
	}
}

// S3-T105, the part of st2 — Req: SRS — EVM Connector §3.1, §3.2 Security; S3 D-16. The endpoint URLs carry a
// fake key generated at run time in the path and the query; they appear in no error and no configuration dump.
// The failed runs on each endpoint come in st3.
func TestT105_EVMEndpointsNeverPrinted(t *testing.T) {
	key := hex.EncodeToString(randomBytes(t, 16))
	primary := "https://rpc.example.invalid/v2/" + key + "?apikey=" + key
	fallback := "http://127.0.0.1:8545/" + key + "?token=" + key
	names := func(env map[string]string) []string {
		var out []string
		for name := range env {
			out = append(out, name)
		}
		return out
	}

	t.Run("read by source code", func(t *testing.T) {
		env := testEnv(t)
		env["EVM_RPC_URL_BASE_SEPOLIA"] = primary
		env["EVM_RPC_FALLBACK_URL_BASE_SEPOLIA"] = fallback
		env["EVM_RPC_URL_ANVIL"] = fallback
		env["EVM_RPC_FALLBACK_URL_SOME_NET_2"] = primary // a fallback without a primary is kept: no endpoint
		env["EVM_RPC_URL_UNSET"] = ""                    // empty counts as unset
		cfg, err := Load(getenvFrom(env), names(env)...)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.EVMRPC) != 3 {
			t.Errorf("EVMRPC has %d sources, want 3", len(cfg.EVMRPC))
		}
		for code, want := range map[string][2]string{
			"base-sepolia": {primary, fallback},
			"anvil":        {fallback, ""},
			"some-net-2":   {"", primary},
		} {
			e := cfg.EVMRPC[code]
			if e.Primary.Value() != want[0] || e.Fallback.Value() != want[1] {
				t.Errorf("%s: endpoints differ from the environment", code)
			}
		}

		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("server starting", "config", cfg)
		var line struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]string{
			"EVM_RPC_URL_ANVIL":                 "set",
			"EVM_RPC_FALLBACK_URL_ANVIL":        "unset",
			"EVM_RPC_URL_BASE_SEPOLIA":          "set",
			"EVM_RPC_FALLBACK_URL_BASE_SEPOLIA": "set",
			"EVM_RPC_URL_SOME_NET_2":            "unset",
			"EVM_RPC_FALLBACK_URL_SOME_NET_2":   "set",
		} {
			if got := line.Config[name]; got != want {
				t.Errorf("dump %s = %v, want %s", name, got, want)
			}
		}
		if _, ok := line.Config["EVM_RPC_URL_UNSET"]; ok {
			t.Error("an empty variable is in the dump")
		}
		assertNoValues(t, buf.String(), key, primary, fallback, "rpc.example.invalid")
		assertNoValues(t, fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg), key)
	})

	t.Run("malformed value stops the start", func(t *testing.T) {
		for name, value := range map[string]string{
			"EVM_RPC_URL_ANVIL":                 "ws://127.0.0.1:8545/" + key,
			"EVM_RPC_FALLBACK_URL_BASE_SEPOLIA": "rpc.example.invalid/" + key,
			"EVM_RPC_URL_BASE_SEPOLIA":          "https:///v2/" + key,
			"EVM_RPC_URL_ANVIL_2":               "https://rpc.example.invalid/%zz" + key,
			"EVM_RPC_URL_base_sepolia":          primary, // the suffix is upper case
			"EVM_RPC_URL_":                      primary,
		} {
			env := testEnv(t)
			env[name] = value
			_, err := Load(getenvFrom(env), names(env)...)
			if err == nil {
				t.Errorf("%s: Load accepted a malformed value", name)
				continue
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s", err, name)
			}
			assertNoValues(t, err.Error(), key, value, "rpc.example.invalid", env["DATABASE_URL"])
		}
	})
}
