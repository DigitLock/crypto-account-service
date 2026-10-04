// Package health serves the health port of server: /healthz, /readyz and /metrics (SRS — Core §3.1, §2.5.1).
package health

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ReadyTimeout bounds all readiness checks of one /readyz request.
const ReadyTimeout = 2 * time.Second

// ReadinessCheck is one condition of /readyz. The process is ready when every check returns nil.
type ReadinessCheck interface {
	Name() string
	Check(ctx context.Context) error
}

// Pinger is the part of a database pool that DatabasePing needs; *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// DatabasePing is ready when the database answers a ping.
type DatabasePing struct {
	DB Pinger
}

// Name implements ReadinessCheck.
func (DatabasePing) Name() string { return "database" }

// Check implements ReadinessCheck.
func (d DatabasePing) Check(ctx context.Context) error { return d.DB.Ping(ctx) }

// RowQuerier is the part of a database pool that SchemaVersion needs; *pgxpool.Pool satisfies it.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SchemaVersion is ready when schema_migrations holds the version the binary expects and is not dirty.
type SchemaVersion struct {
	DB   RowQuerier
	Want uint
}

// Name implements ReadinessCheck.
func (SchemaVersion) Name() string { return "schema" }

// Check implements ReadinessCheck.
func (s SchemaVersion) Check(ctx context.Context) error {
	var (
		version int64
		dirty   bool
	)
	if err := s.DB.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	if dirty {
		return fmt.Errorf("schema version %d is dirty", version)
	}
	if version != int64(s.Want) {
		return fmt.Errorf("schema version %d, binary expects %d", version, s.Want)
	}
	return nil
}

// NewRegistry returns the server's own metrics registry with the Go and process collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// NewHandler returns the routes of the health port.
func NewHandler(logger *slog.Logger, metrics prometheus.Gatherer, checks ...ReadinessCheck) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), ReadyTimeout)
		defer cancel()
		for _, c := range checks {
			if err := c.Check(ctx); err != nil {
				logger.WarnContext(ctx, "readiness check failed", "check", c.Name(), "error", err)
				writeText(w, http.StatusServiceUnavailable, "not ready\n")
				return
			}
		}
		writeText(w, http.StatusOK, "ready\n")
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics, promhttp.HandlerOpts{}))
	return mux
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
