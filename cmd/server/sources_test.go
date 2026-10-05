package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// running is a server started by run on the test database, as cas_server.
type running struct {
	client casv1.ConnectionServiceClient
	logs   *syncBuffer
	// stop ends the server and waits for run to return; the end of the test calls it too.
	stop func()
}

// startServer runs the server with env until stop or the end of the test.
func startServer(t *testing.T, env map[string]string) running {
	t.Helper()
	logs := &syncBuffer{}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(runCtx, getenvFrom(env), logs) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("run did not stop")
			}
		})
	}
	t.Cleanup(stop)
	waitFor(t, "http://127.0.0.1:"+env["HEALTH_HTTP_PORT"]+"/healthz", 200)

	conn, err := grpc.NewClient("127.0.0.1:"+env["GRPC_PORT"], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return running{client: casv1.NewConnectionServiceClient(conn), logs: logs, stop: stop}
}

// databaseEnv is testEnv on the test database, as cas_server, with a tenant and its token.
func databaseEnv(t *testing.T) (map[string]string, context.Context, *registry.Registry) {
	t.Helper()
	owner := testdb.Open(t)
	testdb.Clean(t)
	reg := registry.New(owner)
	if _, err := reg.CreateTenant(context.Background(), "tenant-a"); err != nil {
		t.Fatal(err)
	}
	tok, err := reg.IssueToken(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	env := testEnv(t)
	env["DATABASE_URL"] = testdb.ServerURL(t)
	return env, metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+tok.Value), reg
}

func sourceCodes(t *testing.T, r running, callCtx context.Context) []string {
	t.Helper()
	resp, err := r.client.ListSources(callCtx, &casv1.ListSourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, s := range resp.GetSources() {
		codes = append(codes, s.GetCode())
	}
	return codes
}

func assertSourceDisabled(t *testing.T, what string, err error) {
	t.Helper()
	st := grpcstatus.Convert(err)
	var reason string
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reason = info.GetDomain() + "/" + info.GetReason()
		}
	}
	if st.Code() != codes.FailedPrecondition || reason != "cas/SOURCE_DISABLED" {
		t.Errorf("%s: %v %q, want FAILED_PRECONDITION cas/SOURCE_DISABLED", what, st.Code(), reason)
	}
}

// randomHex returns n hexadecimal characters generated at run time.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)[:n]
}

// C1-T535 — Req: FR-318, EC-318
func TestT535_ChainOutsideTheAllowList(t *testing.T) {
	env, callCtx, _ := databaseEnv(t)
	env["EVM_ALLOWED_CHAIN_IDS"] = "31337"
	r := startServer(t, env)

	var warning string
	for line := range strings.Lines(r.logs.String()) {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, `"source":"base-sepolia"`) {
			warning = line
		}
	}
	if !strings.Contains(warning, `"chain_id":84532`) || strings.Contains(r.logs.String(), `"source":"anvil"`) {
		t.Errorf("start log: want one warning for base-sepolia with chain_id 84532 and none for anvil:\n%s",
			r.logs.String())
	}
	if !strings.Contains(r.logs.String(), `"EVM_ALLOWED_CHAIN_IDS":"31337"`) {
		t.Error("the start log line does not show EVM_ALLOWED_CHAIN_IDS")
	}

	if got := sourceCodes(t, r, callCtx); strings.Join(got, ",") != "anvil" {
		t.Errorf("sources = %v, want [anvil]", got)
	}
	_, err := r.client.CreateConnection(callCtx, &casv1.CreateConnectionRequest{
		OwnerRef: "owner-1", Source: "base-sepolia",
		Credential: &casv1.CreateConnectionRequest_Wallet{Wallet: &casv1.Wallet{Address: "0x" + randomHex(t, 40)}},
	})
	assertSourceDisabled(t, "create on base-sepolia", err)
}

// C1-T536 — Req: §2.1.1, §2.4, §3.1. The flag of the server; cmd/casctl runs source add-fake itself.
func TestT536_FakeSourceBehindItsFlag(t *testing.T) {
	env, callCtx, reg := databaseEnv(t)
	for i, want := range []bool{true, false} {
		seed, err := reg.AddFakeSource(context.Background())
		if err != nil || seed.SourceAdded != want {
			t.Fatalf("AddFakeSource run %d: source added %v, %v; want %v", i+1, seed.SourceAdded, err, want)
		}
	}
	request := &casv1.CreateConnectionRequest{
		OwnerRef: "owner-1", Source: "fake",
		Credential: &casv1.CreateConnectionRequest_ExchangeKey{ExchangeKey: &casv1.ExchangeKey{
			ApiKey: randomHex(t, 32), ApiSecret: randomHex(t, 64),
		}},
	}

	t.Run("without ENABLE_FAKE_SOURCE", func(t *testing.T) {
		r := startServer(t, env)
		if got := sourceCodes(t, r, callCtx); strings.Contains(strings.Join(got, ","), "fake") {
			t.Errorf("sources = %v, want no fake", got)
		}
		_, err := r.client.CreateConnection(callCtx, request)
		assertSourceDisabled(t, "create on fake", err)
	})

	t.Run("with ENABLE_FAKE_SOURCE", func(t *testing.T) {
		withFlag := testEnv(t)
		for k, v := range env {
			if k != "GRPC_PORT" && k != "HEALTH_HTTP_PORT" {
				withFlag[k] = v
			}
		}
		withFlag["ENABLE_FAKE_SOURCE"] = "true"
		r := startServer(t, withFlag)
		if got := sourceCodes(t, r, callCtx); strings.Join(got, ",") != "anvil,base-sepolia,fake" {
			t.Errorf("sources = %v, want anvil, base-sepolia, fake", got)
		}
		if _, err := r.client.CreateConnection(callCtx, request); err != nil {
			t.Errorf("create on fake: %v", err)
		}
	})
}

// C1-T626 — Req: §3.2. Two servers through run on the test database, with real ticks of 100 ms.
func TestT626_OneEngine(t *testing.T) {
	envA, callCtx, reg := databaseEnv(t)
	if _, err := reg.AddFakeSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"ENABLE_FAKE_SOURCE": "true", "SYNC_TICK": "100ms", "SYNC_LOCK_RETRY": "200ms", "LOG_LEVEL": "debug",
	} {
		envA[k] = v
	}
	envB := testEnv(t) // its own ports
	for k, v := range envA {
		if k != "GRPC_PORT" && k != "HEALTH_HTTP_PORT" {
			envB[k] = v
		}
	}
	a, b := startServer(t, envA), startServer(t, envB)
	if _, err := a.client.CreateConnection(callCtx, &casv1.CreateConnectionRequest{
		OwnerRef: "owner-1", Source: "fake",
		Credential: &casv1.CreateConnectionRequest_ExchangeKey{ExchangeKey: &casv1.ExchangeKey{
			ApiKey: randomHex(t, 32), ApiSecret: randomHex(t, 64),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	runs := func(r running) bool { return strings.Contains(r.logs.String(), `"msg":"sync run finished"`) }
	holds := func(r running) bool { return strings.Contains(r.logs.String(), `"msg":"engine lock taken"`) }
	waitUntil(t, "one server runs the streams", 5*time.Second, func() bool { return runs(a) || runs(b) })
	holder, other := a, b
	if runs(b) {
		holder, other = b, a
	}
	if holds(other) || runs(other) {
		t.Fatalf("both servers took the engine lock or ran streams")
	}
	if _, err := other.client.ListSources(callCtx, &casv1.ListSourcesRequest{}); err != nil {
		t.Errorf("the server without the lock does not serve the API: %v", err)
	}

	holder.stop()
	waitUntil(t, "the other server takes the lock within SYNC_LOCK_RETRY", 2*time.Second, func() bool { return holds(other) })
	if _, err := testdb.Open(t).Exec(context.Background(), `UPDATE sync_cursors SET next_run_at = now()`); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the other server runs the streams", 2*time.Second, func() bool { return runs(other) })
}

// waitUntil polls cond every 10 ms until within.
func waitUntil(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %v", what, within)
}
