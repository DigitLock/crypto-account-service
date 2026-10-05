// Command server is the Core service of CAS: gRPC API on GRPC_PORT, health and metrics on HEALTH_HTTP_PORT.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/connector/fake"
	"github.com/DigitLock/crypto-account-service/internal/grpc/api"
	"github.com/DigitLock/crypto-account-service/internal/health"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/vault"
	"github.com/DigitLock/crypto-account-service/migrations"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stderr)
	stop()
	if err != nil {
		// Errors of run name variables and never carry a secret.
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

// run starts the servers and blocks until ctx is cancelled or a server fails.
// The database is not pinged here: an unreachable database makes /readyz answer 503.
func run(ctx context.Context, getenv func(string) string, stderr io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	logger := newLogger(cfg, stderr)
	slog.SetDefault(logger)

	pool, err := newPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	healthLn, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.HealthHTTPPort))
	if err != nil {
		return fmt.Errorf("listen on HEALTH_HTTP_PORT: %w", err)
	}
	grpcLn, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.GRPCPort))
	if err != nil {
		_ = healthLn.Close()
		return fmt.Errorf("listen on GRPC_PORT: %w", err)
	}

	healthSrv := &http.Server{
		Handler: health.NewHandler(logger, health.NewRegistry(),
			health.DatabasePing{DB: pool},
			health.SchemaVersion{DB: pool, Want: migrations.Latest()},
		),
		ReadHeaderTimeout: 5 * time.Second,
	}
	v, err := vault.New(cfg.MasterKey.Value(), cfg.MasterKeyVersion)
	if err != nil {
		_ = healthLn.Close()
		_ = grpcLn.Close()
		return err
	}
	connectors := newConnectors(cfg)
	grpcSrv := api.NewServer(api.Deps{
		Credentials: repository.New(pool),
		Connections: registry.NewConnections(pool, v, connectors, time.Now),
		Logger:      logger,
	})

	logger.Info("server starting", "config", cfg)
	warnUnavailableSources(ctx, logger, pool, connectors)

	errc := make(chan error, 2)
	go func() {
		if err := healthSrv.Serve(healthLn); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("health server: %w", err)
			return
		}
		errc <- nil
	}()
	go func() {
		if err := grpcSrv.Serve(grpcLn); err != nil {
			errc <- fmt.Errorf("gRPC server: %w", err)
			return
		}
		errc <- nil
	}()

	var serveErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case serveErr = <-errc:
		logger.Error("server failed", "error", serveErr)
	}

	shutdownErr := shutdown(cfg.ShutdownTimeout, healthSrv, grpcSrv)
	logger.Info("server stopped")
	return errors.Join(serveErr, shutdownErr)
}

// newConnectors registers the EVM connector with the allow-list and, only with ENABLE_FAKE_SOURCE,
// the fake connector.
func newConnectors(cfg config.Config) *connector.Set {
	set := connector.NewSet()
	set.RegisterEVM(evm.New(cfg.EVMAllowedChainIDs))
	if cfg.EnableFakeSource {
		set.Register(fake.Code, fake.New())
	}
	return set
}

// warnUnavailableSources logs one line per enabled EVM source that is not available (EC-318).
// Availability is evaluated on every request; this is information for the operator only.
func warnUnavailableSources(ctx context.Context, logger *slog.Logger, db repository.DBTX, connectors *connector.Set) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := repository.New(db).ListSources(ctx)
	if err != nil {
		logger.Warn("cannot read the sources at start; the start goes on", "error", err)
		return
	}
	for _, row := range rows {
		src := connector.Source{Code: row.Code, Kind: row.Kind, Enabled: row.Enabled, Config: row.Config}
		if src.Kind != connector.KindEVM || !src.Enabled || connectors.Available(src) {
			continue
		}
		if chainID, ok := connector.ChainID(src); ok {
			logger.Warn("EVM source not available: its chain ID is outside EVM_ALLOWED_CHAIN_IDS",
				"source", src.Code, "chain_id", chainID)
		} else {
			logger.Warn("EVM source not available: chain_id is missing or malformed in sources.config",
				"source", src.Code, "chain_id", "invalid")
		}
	}
}

// shutdown stops both servers within timeout: graceful first, then forced.
func shutdown(timeout time.Duration, healthSrv *http.Server, grpcSrv *grpc.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	grpcDone := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(grpcDone)
	}()

	var err error
	if e := healthSrv.Shutdown(ctx); e != nil {
		err = fmt.Errorf("health server shutdown: %w", e)
		_ = healthSrv.Close()
	}
	select {
	case <-grpcDone:
	case <-ctx.Done():
		grpcSrv.Stop()
		<-grpcDone
		err = errors.Join(err, errors.New("gRPC server: forced stop after SHUTDOWN_TIMEOUT"))
	}
	return err
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == config.LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// newPool creates the pool without connecting. The parse error is not wrapped:
// it would quote parts of DATABASE_URL.
func newPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL.Value())
	if err != nil {
		return nil, errors.New("config: DATABASE_URL is not a valid PostgreSQL connection string")
	}
	pc.MaxConns = cfg.DBPoolMaxConns
	pc.MinConns = cfg.DBPoolMinConns
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, errors.New("database: cannot create the connection pool")
	}
	return pool, nil
}
