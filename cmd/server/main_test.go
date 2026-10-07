package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
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

// freePort returns a loopback port that nothing listens on.
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

// testEnv returns a complete environment: a master key generated at run time and a database
// on a loopback port that nothing listens on.
func testEnv(t *testing.T) map[string]string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	db := url.URL{
		Scheme:   "postgres",
		User:     url.User("cas"),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t))),
		Path:     "/cas",
		RawQuery: "connect_timeout=1",
	}
	return map[string]string{
		"CAS_MASTER_KEY":   base64.StdEncoding.EncodeToString(key),
		"DATABASE_URL":     db.String(),
		"GRPC_PORT":        strconv.Itoa(freePort(t)),
		"HEALTH_HTTP_PORT": strconv.Itoa(freePort(t)),
		"SHUTDOWN_TIMEOUT": "5s",
	}
}

func getenvFrom(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func runWithTimeout(t *testing.T, env map[string]string, stderr io.Writer) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return run(ctx, getenvFrom(env), stderr)
}

// C1-T101 — Req: §3.1
func TestT101_RunWithoutRequiredVariable(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "CAS_MASTER_KEY"} {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			secrets := []string{env["CAS_MASTER_KEY"], env["DATABASE_URL"]}
			delete(env, name)

			var stderr bytes.Buffer
			err := runWithTimeout(t, env, &stderr)
			if err == nil {
				t.Fatal("run succeeded without " + name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s", err, name)
			}
			for _, s := range secrets {
				if s != "" && (strings.Contains(err.Error(), s) || strings.Contains(stderr.String(), s)) {
					t.Error("a configuration value was printed")
				}
			}
		})
	}
}

// C1-T103 — Req: §3.1, ADR-4
func TestT103_RunWithInvalidConfiguration(t *testing.T) {
	cases := map[string][2]string{
		"non-numeric port":     {"GRPC_PORT", "grpc"},
		"master key 16 bytes":  {"CAS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16))},
		"malformed duration":   {"SHUTDOWN_TIMEOUT", "15"},
		"invalid DATABASE_URL": {"DATABASE_URL", "not-a-connection-string"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			env[c[0]] = c[1]
			err := runWithTimeout(t, env, io.Discard)
			if err == nil {
				t.Fatalf("run accepted an invalid %s", c[0])
			}
			if !strings.Contains(err.Error(), c[0]) {
				t.Errorf("error %q does not name %s", err, c[0])
			}
			if strings.Contains(err.Error(), c[1]) {
				t.Errorf("error %q prints the value", err)
			}
		})
	}
}

// C1-T104 — Req: §3.1, §2.5.1. The whole server: health port, gRPC reflection, shutdown.
// The database is unreachable: the server still starts and /readyz answers 503.
func TestT104_RunServesHealthPortAndGRPC(t *testing.T) {
	env := testEnv(t)
	healthURL := "http://127.0.0.1:" + env["HEALTH_HTTP_PORT"]
	var logs syncBuffer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, getenvFrom(env), &logs) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, healthURL+"/healthz", http.StatusOK)
	for path, want := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusServiceUnavailable,
		"/metrics": http.StatusOK,
	} {
		if got := status(t, healthURL+path); got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}

	conn, err := grpc.NewClient("127.0.0.1:"+env["GRPC_PORT"], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var services []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		if !strings.HasPrefix(s.GetName(), "grpc.reflection.") {
			services = append(services, s.GetName())
		}
	}
	slices.Sort(services)
	// CardService since S2 st4.
	if want := []string{"cas.v1.AccountDataService", "cas.v1.CardService", "cas.v1.ConnectionService"}; !slices.Equal(services, want) {
		t.Errorf("gRPC services = %v, want %v", services, want)
	}
	_ = stream.CloseSend()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v after shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop within SHUTDOWN_TIMEOUT")
	}
	done <- nil // for the cleanup

	out := logs.String()
	if !strings.Contains(out, `"msg":"server starting"`) || !strings.Contains(out, `"GRPC_PORT":`+env["GRPC_PORT"]) {
		t.Errorf("start log line with the configuration missing:\n%s", out)
	}
	for _, s := range []string{env["CAS_MASTER_KEY"], env["DATABASE_URL"], "CAS_MASTER_KEY\"", "DATABASE_URL"} {
		if strings.Contains(out, s) {
			t.Errorf("log contains %q", s)
		}
	}
}

func status(t *testing.T, u string) int {
	t.Helper()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitFor(t *testing.T, u string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(u); err == nil {
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not answer %d within 5 s", u, want)
}
