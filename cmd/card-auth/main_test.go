package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// syncBuffer is a log sink that run and the test may use at the same time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// client does not keep connections: a spare idle connection would hold up the graceful shutdown.
var client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// unreachableDatabaseURL points at a loopback port that nothing listens on, with a password generated at run time.
func unreachableDatabaseURL(t *testing.T) string {
	t.Helper()
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("cas_card_auth", randomHex(t, 12)),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t))),
		Path:     "/cas",
		RawQuery: "connect_timeout=1",
	}
	return u.String()
}

// unreachableRPCURL points at a loopback port that nothing listens on, with a path like a provider API key.
func unreachableRPCURL(t *testing.T) string {
	t.Helper()
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t))) + "/v2/" + randomHex(t, 16)
}

// testEnv returns a complete environment of card-auth on c and the database databaseURL.
func testEnv(t *testing.T, c *testchain.Chain, databaseURL string) map[string]string {
	t.Helper()
	return map[string]string{
		"CARD_AUTH_DATABASE_URL":       databaseURL,
		"OPERATOR_PRIVATE_KEY":         "0x" + hex.EncodeToString(c.OperatorKey.Value()),
		"CARD_AUTH_CHAIN_ID":           strconv.Itoa(testchain.ChainID),
		"CARD_AUTH_RPC_URL":            c.RPCURL,
		"CARD_AUTH_CONTROLLER_ADDRESS": c.Controller.Hex(),
		"CARD_AUTH_TOKEN_ADDRESS":      c.Token.Hex(),
		"CARD_AUTH_TOKEN_DECIMALS":     strconv.Itoa(testchain.TokenDecimals),
		"CARD_AUTH_HTTP_PORT":          strconv.Itoa(freePort(t)),
		"CARD_AUTH_HEALTH_PORT":        strconv.Itoa(freePort(t)),
		"CARD_AUTH_SHUTDOWN_TIMEOUT":   "5s",
	}
}

func getenvFrom(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// runFailing runs card-auth and expects it to stop with an error before it serves.
func runFailing(t *testing.T, env map[string]string, stderr io.Writer) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, getenvFrom(env), stderr)
	if err == nil {
		t.Fatal("run started")
	}
	t.Logf("run: %v", err)
	if env["CARD_AUTH_HEALTH_PORT"] != "" {
		if conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+env["CARD_AUTH_HEALTH_PORT"], time.Second); dialErr == nil {
			conn.Close()
			t.Error("the health port is still listening after a failed start")
		}
	}
	return err
}

// started is a running card-auth.
type started struct {
	healthURL, httpURL string
	logs               *syncBuffer
	stop               func() error
}

// start runs card-auth until stop or the end of the test and waits for /healthz.
func start(t *testing.T, env map[string]string) *started {
	t.Helper()
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, getenvFrom(env), logs) }()

	var once sync.Once
	var runErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(10 * time.Second):
				runErr = errors.New("run did not stop within CARD_AUTH_SHUTDOWN_TIMEOUT")
			}
		})
		return runErr
	}
	t.Cleanup(func() { _ = stop() })

	s := &started{
		healthURL: "http://127.0.0.1:" + env["CARD_AUTH_HEALTH_PORT"],
		httpURL:   "http://127.0.0.1:" + env["CARD_AUTH_HTTP_PORT"],
		logs:      logs,
		stop:      stop,
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if resp, err := client.Get(s.healthURL + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return s
			}
		}
		select {
		case err := <-done:
			t.Fatalf("run stopped before it served: %v\n%s", err, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz did not answer 200 within 30 s\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func get(t *testing.T, u string) (int, http.Header) {
	t.Helper()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// S2-T104 — Req: SRS — Card Spend §3.1. /readyz runs as cas_card_auth on a migrated database.
func TestT104_Health(t *testing.T) {
	testdb.Open(t) // migrates as the owner role
	databaseURL := testdb.CardAuthURL(t)
	c := testchain.Start(t)

	t.Run("database reachable", func(t *testing.T) {
		s := start(t, testEnv(t, c, databaseURL))
		for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/metrics": 200} {
			status, header := get(t, s.healthURL+path)
			if status != want {
				t.Errorf("GET %s = %d, want %d\n%s", path, status, want, s.logs.String())
			}
			if path == "/metrics" && !strings.HasPrefix(header.Get("Content-Type"), "text/plain") {
				t.Errorf("/metrics Content-Type = %q, want the Prometheus text format", header.Get("Content-Type"))
			}
		}
		for _, path := range []string{"/", "/v1/authorizations", "/healthz"} {
			if status, _ := get(t, s.httpURL+path); status != http.StatusNotFound {
				t.Errorf("GET %s on the HTTP port = %d, want 404", path, status)
			}
		}
		if err := s.stop(); err != nil {
			t.Errorf("run returned %v after shutdown", err)
		}
		if out := s.logs.String(); !strings.Contains(out, `"msg":"card-auth stopped"`) {
			t.Errorf("no stop line in the log:\n%s", out)
		}
	})

	t.Run("database unreachable", func(t *testing.T) {
		s := start(t, testEnv(t, c, unreachableDatabaseURL(t)))
		if status, _ := get(t, s.healthURL+"/healthz"); status != http.StatusOK {
			t.Errorf("GET /healthz = %d, want 200", status)
		}
		if status, _ := get(t, s.healthURL+"/readyz"); status != http.StatusServiceUnavailable {
			t.Errorf("GET /readyz = %d, want 503", status)
		}
		if err := s.stop(); err != nil {
			t.Errorf("run returned %v after shutdown", err)
		}
	})
}

// S2-T105 — Req: SRS — Card Spend §3.2 Security. Start, configuration dump and start errors at LOG_LEVEL=debug.
// The authorization part of the row (one authorization, one failed send) follows in st5.
func TestT105_OperatorKeyNeverPrinted(t *testing.T) {
	c := testchain.Start(t)
	rpcHost := strings.TrimPrefix(c.RPCURL, "http://")

	newEnv := func(t *testing.T) map[string]string {
		env := testEnv(t, c, unreachableDatabaseURL(t))
		env["LOG_LEVEL"] = "debug"
		return env
	}
	// forbidden returns every value that must not appear: the key in each form, the URLs and their hosts.
	forbidden := func(env map[string]string) []string {
		key := strings.TrimPrefix(env["OPERATOR_PRIVATE_KEY"], "0x")
		out := []string{key, strings.ToUpper(key), rpcHost}
		if raw, err := hex.DecodeString(key); err == nil && len(raw) > 0 {
			out = append(out, string(raw))
		}
		for _, name := range []string{"CARD_AUTH_DATABASE_URL", "CARD_AUTH_RPC_URL", "CARD_AUTH_RPC_FALLBACK_URL"} {
			if v := env[name]; v != "" {
				out = append(out, v)
				if u, err := url.Parse(v); err == nil {
					out = append(out, u.Host, u.Path)
					if p, ok := u.User.Password(); ok {
						out = append(out, p)
					}
				}
			}
		}
		return out
	}
	assertClean := func(t *testing.T, what, s string, env map[string]string) {
		t.Helper()
		for _, v := range forbidden(env) {
			if len(v) >= 4 && strings.Contains(s, v) {
				t.Errorf("%s contains a secret value or a URL: %s", what, s)
			}
		}
	}

	t.Run("start and configuration dump", func(t *testing.T) {
		env := newEnv(t)
		s := start(t, env)
		if err := s.stop(); err != nil {
			t.Fatal(err)
		}
		out := s.logs.String()
		if !strings.Contains(out, `"msg":"card-auth starting"`) || !strings.Contains(out, `"CARD_AUTH_CHAIN_ID":31337`) ||
			!strings.Contains(out, `"operator":"`+c.Operator.Hex()+`"`) {
			t.Errorf("start line with the configuration and the operator address missing:\n%s", out)
		}
		assertClean(t, "log", out, env)
		for _, name := range []string{"OPERATOR_PRIVATE_KEY", "CARD_AUTH_DATABASE_URL", "CARD_AUTH_RPC_URL"} {
			if strings.Contains(out, name) {
				t.Errorf("log names %s", name)
			}
		}
	})

	errorCases := map[string]func(t *testing.T, env map[string]string){
		"key that is not a secp256k1 key": func(t *testing.T, env map[string]string) {
			env["OPERATOR_PRIVATE_KEY"] = strings.Repeat("0", 64)
		},
		"key of 31 bytes": func(t *testing.T, env map[string]string) {
			env["OPERATOR_PRIVATE_KEY"] = env["OPERATOR_PRIVATE_KEY"][:64]
		},
		"database URL malformed": func(t *testing.T, env map[string]string) {
			env["CARD_AUTH_DATABASE_URL"] = "postgres://cas:" + randomHex(t, 8) + "@[::1"
		},
		"RPC unreachable": func(t *testing.T, env map[string]string) {
			env["CARD_AUTH_RPC_URL"] = unreachableRPCURL(t)
		},
		"fallback unreachable": func(t *testing.T, env map[string]string) {
			env["CARD_AUTH_RPC_FALLBACK_URL"] = unreachableRPCURL(t)
		},
		"RPC URL malformed": func(t *testing.T, env map[string]string) {
			env["CARD_AUTH_RPC_URL"] = "ftp://" + randomHex(t, 8)
		},
		"wrong decimals": func(t *testing.T, env map[string]string) {
			env["CARD_AUTH_TOKEN_DECIMALS"] = "18"
		},
	}
	for name, change := range errorCases {
		t.Run("start error: "+name, func(t *testing.T) {
			env := newEnv(t)
			change(t, env)
			var stderr syncBuffer
			err := runFailing(t, env, &stderr)
			assertClean(t, "error", err.Error(), env)
			assertClean(t, "log", stderr.String(), env)
		})
	}
}

// S2-T106 — Req: SRS — Card Spend §3.2 Chain access
func TestT106_StartChecks(t *testing.T) {
	c := testchain.Start(t)
	otherChain := testchain.StartAnvil(t, 31338)

	cases := []struct {
		name   string
		change func(env map[string]string)
		check  string
	}{
		{"primary on another chain", func(env map[string]string) {
			env["CARD_AUTH_RPC_URL"] = otherChain
			env["EVM_ALLOWED_CHAIN_IDS"] = "31337,31338"
		}, "eth_chainId of CARD_AUTH_RPC_URL"},
		{"fallback on another chain", func(env map[string]string) {
			env["CARD_AUTH_RPC_FALLBACK_URL"] = otherChain
			env["EVM_ALLOWED_CHAIN_IDS"] = "31337,31338"
		}, "eth_chainId of CARD_AUTH_RPC_FALLBACK_URL"},
		{"wrong token address", func(env map[string]string) {
			env["CARD_AUTH_TOKEN_ADDRESS"] = c.Treasury.Hex()
		}, "token() of the controller"},
		{"controller without code", func(env map[string]string) {
			env["CARD_AUTH_CONTROLLER_ADDRESS"] = c.Admin.Hex()
		}, "token() of the controller"},
		{"wrong decimals", func(env map[string]string) {
			env["CARD_AUTH_TOKEN_DECIMALS"] = "18"
		}, "decimals() of the token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t, c, unreachableDatabaseURL(t))
			tc.change(env)
			err := runFailing(t, env, io.Discard)
			var checkErr *chain.StartCheckError
			if !errors.As(err, &checkErr) || checkErr.Check != tc.check {
				t.Fatalf("run = %v, want the start check %q", err, tc.check)
			}
			for _, u := range []string{c.RPCURL, otherChain, strings.TrimPrefix(otherChain, "http://")} {
				if strings.Contains(err.Error(), u) {
					t.Errorf("error %q contains a URL", err)
				}
			}
		})
	}

	t.Run("correct configuration starts", func(t *testing.T) {
		env := testEnv(t, c, unreachableDatabaseURL(t))
		env["CARD_AUTH_RPC_FALLBACK_URL"] = c.RPCURL
		s := start(t, env)
		if err := s.stop(); err != nil {
			t.Errorf("run returned %v after shutdown", err)
		}
	})

	t.Run("nothing sent", func(t *testing.T) {
		rc, err := rpc.Dial(c.RPCURL)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		var nonce hexutil.Uint64
		if err := rc.Call(&nonce, "eth_getTransactionCount", c.Operator, "pending"); err != nil {
			t.Fatal(err)
		}
		if nonce != 0 {
			t.Errorf("the operator sent %d transactions", nonce)
		}
	})
}
