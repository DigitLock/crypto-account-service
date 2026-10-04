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
	"google.golang.org/grpc/reflection"

	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/health"
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
	grpcSrv := grpc.NewServer()
	reflection.Register(grpcSrv)

	logger.Info("server starting", "config", cfg)

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
