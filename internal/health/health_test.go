package health

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

func newTestServer(t *testing.T, checks ...ReadinessCheck) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewHandler(slog.New(slog.DiscardHandler), NewRegistry(), checks...))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, u string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// unreachableDatabaseURL points at a loopback port that nothing listens on.
func unreachableDatabaseURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "postgres", User: url.User("cas"), Host: addr, Path: "/cas", RawQuery: "connect_timeout=1"}
	return u.String()
}

func newPool(t *testing.T, connString string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), connString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// blockingCheck answers only when its context ends.
type blockingCheck struct{}

func (blockingCheck) Name() string { return "blocking" }
func (blockingCheck) Check(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// C1-T104 — Req: §3.1
func TestT104_Healthz(t *testing.T) {
	srv := newTestServer(t, blockingCheck{})

	status, _, body := get(t, srv.URL+"/healthz")
	if status != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", status)
	}
	if body != "ok\n" {
		t.Errorf("body = %q", body)
	}

	resp, err := http.Post(srv.URL+"/healthz", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", resp.StatusCode)
	}
}

// C1-T105 — Req: §3.1
func TestT105_ReadyzDatabase(t *testing.T) {
	t.Run("database unreachable", func(t *testing.T) {
		srv := newTestServer(t, DatabasePing{DB: newPool(t, unreachableDatabaseURL(t))})
		if status, _, _ := get(t, srv.URL+"/readyz"); status != http.StatusServiceUnavailable {
			t.Errorf("GET /readyz = %d, want 503", status)
		}
	})

	t.Run("check slower than 2 s", func(t *testing.T) {
		srv := newTestServer(t, blockingCheck{})
		start := time.Now()
		status, _, _ := get(t, srv.URL+"/readyz")
		if status != http.StatusServiceUnavailable {
			t.Errorf("GET /readyz = %d, want 503", status)
		}
		if elapsed := time.Since(start); elapsed < ReadyTimeout || elapsed > ReadyTimeout+time.Second {
			t.Errorf("answered after %v, want about %v", elapsed, ReadyTimeout)
		}
	})

	t.Run("database reachable", func(t *testing.T) {
		srv := newTestServer(t, DatabasePing{DB: newPool(t, testdb.URL(t))})
		if status, _, body := get(t, srv.URL+"/readyz"); status != http.StatusOK {
			t.Errorf("GET /readyz = %d %q, want 200", status, body)
		}
	})
}

// C1-T106 — Req: §2.5.1
func TestT106_Metrics(t *testing.T) {
	srv := newTestServer(t)

	status, header, body := get(t, srv.URL+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", status)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
	if !strings.Contains(body, "# TYPE go_goroutines gauge") {
		t.Error("Go collector metrics missing")
	}
	if runtime.GOOS == "linux" && !strings.Contains(body, "process_start_time_seconds") {
		t.Error("process collector metrics missing")
	}
}
