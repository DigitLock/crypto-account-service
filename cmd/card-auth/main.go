// Command card-auth is the card authorization service of CAS (SRS — Card Spend): processor API on
// CARD_AUTH_HTTP_PORT, health and metrics on CARD_AUTH_HEALTH_PORT.
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

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/crs"
	"github.com/DigitLock/crypto-account-service/internal/debit"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/health"
	opqueue "github.com/DigitLock/crypto-account-service/internal/operator"
	"github.com/DigitLock/crypto-account-service/internal/processorapi"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/returns"
	"github.com/DigitLock/crypto-account-service/internal/signer"
	"github.com/DigitLock/crypto-account-service/internal/tracker"
	"github.com/DigitLock/crypto-account-service/migrations"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stderr)
	stop()
	if err != nil {
		// Errors of run name variables and checks and never carry a secret or a URL.
		fmt.Fprintln(os.Stderr, "card-auth:", err)
		os.Exit(1)
	}
}

// run checks the configuration, the chain and the nonce of the operator, starts the servers and blocks until
// ctx is cancelled or a server fails. The nonce check writes operator_accounts: the database must be reachable
// at start; later an unreachable database makes /readyz answer 503.
func run(ctx context.Context, getenv func(string) string, stderr io.Writer) error {
	cfg, err := config.LoadCardAuth(getenv)
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

	operator, err := signer.New(cfg.OperatorKey)
	if err != nil {
		return err
	}

	client, err := chain.Dial(ctx, "CARD_AUTH_RPC_URL", cfg.RPCURL, "CARD_AUTH_RPC_FALLBACK_URL", cfg.RPCFallbackURL)
	if err != nil {
		return err
	}
	defer client.Close()
	symbol, err := client.StartChecks(ctx, chain.Expected{
		ChainID:    cfg.ChainID,
		Allowed:    cfg.EVMAllowedChainIDs,
		Controller: cfg.ControllerAddress,
		Token:      cfg.TokenAddress,
		Decimals:   cfg.TokenDecimals,
	})
	if err != nil {
		return err
	}
	count, err := client.OperatorNonce(ctx, operator.Address())
	if err != nil {
		return err
	}
	if err := syncNonce(ctx, pool, logger, cfg.ChainID, operator.Address(), count); err != nil {
		return err
	}

	healthLn, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.HealthPort))
	if err != nil {
		return fmt.Errorf("listen on CARD_AUTH_HEALTH_PORT: %w", err)
	}
	httpLn, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.HTTPPort))
	if err != nil {
		_ = healthLn.Close()
		return fmt.Errorf("listen on CARD_AUTH_HTTP_PORT: %w", err)
	}

	reader, err := chain.NewReader(client, chain.ReaderConfig{
		ChainID:       cfg.ChainID,
		Controller:    cfg.ControllerAddress,
		Token:         cfg.TokenAddress,
		ReadTimeout:   cfg.RPCReadTimeout,
		FallbackAfter: cfg.RPCFallbackAfter,
		ProbeInterval: cfg.TrackerInterval,
	}, logger)
	if err != nil {
		return err
	}
	// Unset CRS_ADDRESS: no rate source, every non-USD authorization is RATE_UNAVAILABLE (§3.1).
	var rates decision.Rates
	if cfg.CRSAddress != "" {
		rc, err := crs.Dial(cfg.CRSAddress)
		if err != nil {
			return err
		}
		defer rc.Close()
		rates = rc
	} else {
		logger.Warn("CRS_ADDRESS is unset: every non-USD authorization is declined as RATE_UNAVAILABLE")
	}
	metrics := health.NewRegistry()
	queue := opqueue.New(pool, reader, operator, cfg.ChainID, cfg.RPCReadTimeout, logger, time.Now)
	debitStep, err := debit.New(pool, queue, debit.NewSignals(), debit.NewMetrics(metrics), debit.Config{
		Controller:       cfg.ControllerAddress,
		DecisionDeadline: cfg.DecisionDeadline,
		DebitValidity:    cfg.DebitValidity,
		GasLimit:         cfg.DebitGasLimit,
		PollInterval:     cfg.ReceiptPollInterval,
	}, logger, time.Now)
	if err != nil {
		return err
	}
	engine := decision.New(pool, rates, reader, debitStep, decision.Config{
		DecisionDeadline: cfg.DecisionDeadline,
		QuoteBufferBPS:   cfg.QuoteBufferBPS,
		TokenDecimals:    cfg.TokenDecimals,
		Token:            symbol,
		ChainID:          cfg.ChainID,
		MinSendWindow:    cfg.MinSendWindow,
	}, logger, time.Now)

	healthSrv := &http.Server{
		Handler: health.NewHandler(logger, metrics,
			health.DatabasePing{DB: pool},
			health.SchemaVersion{DB: pool, Want: migrations.Latest()},
		),
		ReadHeaderTimeout: 5 * time.Second,
	}
	httpSrv := &http.Server{
		Handler: processorapi.NewHandler(processorapi.Deps{
			DB:      pool,
			Engine:  engine,
			Returns: returns.New(pool, time.Now),
			Reads:   registry.NewCards(pool, time.Now),
			Metrics: processorapi.NewMetrics(metrics),
			Logger:  logger,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("card-auth starting", "config", cfg, "operator", operator.Address().Hex(), "token", symbol)

	refunds, err := tracker.New(pool, queue, tracker.Config{
		Controller:            cfg.ControllerAddress,
		Token:                 cfg.TokenAddress,
		RefundGasLimit:        cfg.RefundGasLimit,
		Interval:              cfg.TrackerInterval,
		RetryInterval:         cfg.ReturnRetryInterval,
		FinalityMode:          cfg.FinalityMode,
		FinalityTag:           cfg.FinalityTag,
		FinalityConfirmations: cfg.FinalityConfirmations,
	}, tracker.NewMetrics(metrics), logger, time.Now)
	if err != nil {
		return err
	}

	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	go reader.Probe(probeCtx)
	go refunds.Run(probeCtx)

	errc := make(chan error, 2)
	serve := func(name string, srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("%s: %w", name, err)
			return
		}
		errc <- nil
	}
	go serve("health server", healthSrv, healthLn)
	go serve("HTTP server", httpSrv, httpLn)

	var serveErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case serveErr = <-errc:
		logger.Error("server failed", "error", serveErr)
	}

	shutdownErr := shutdown(cfg.ShutdownTimeout, healthSrv, httpSrv)
	logger.Info("card-auth stopped")
	return errors.Join(serveErr, shutdownErr)
}

// shutdown stops both servers within timeout: graceful first, then forced.
func shutdown(timeout time.Duration, servers ...*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var err error
	for _, srv := range servers {
		if e := srv.Shutdown(ctx); e != nil {
			err = errors.Join(err, fmt.Errorf("shutdown: %w", e))
			_ = srv.Close()
		}
	}
	return err
}

func newLogger(cfg config.CardAuth, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == config.LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// newPool creates the pool without connecting. The parse error is not wrapped:
// it would quote parts of CARD_AUTH_DATABASE_URL.
func newPool(ctx context.Context, cfg config.CardAuth) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL.Value())
	if err != nil {
		return nil, errors.New("config: CARD_AUTH_DATABASE_URL is not a valid PostgreSQL connection string")
	}
	pc.MaxConns = cfg.DBPoolMaxConns
	pc.MinConns = cfg.DBPoolMinConns
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, errors.New("database: cannot create the connection pool")
	}
	return pool, nil
}
