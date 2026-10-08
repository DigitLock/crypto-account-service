// Package reconciler is the reconciliation worker of server (SRS — Core §3.2; SRS — Card Spend UC-4 trigger;
// S3 D-8): a companion of the engine that, every SYNC_TICK while the engine lock is held, reconciles each EVM source
// whose treasury logs cursor moved since its newest run, and keeps the gauge reconciliation_mismatches (S3 D-9).
// It reads and writes the database only, with the rights of cas_server.
package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/reconcile"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// MismatchTypes are the values of the label type of reconciliation_mismatches: the mismatch types of SRS — Core
// §2.1.1, in the order of the proto enum.
var MismatchTypes = []string{
	reconcile.MissingDebit, reconcile.AmountMismatch, reconcile.UnknownDebit, reconcile.UnexpectedDebit,
	reconcile.MissingRefund, reconcile.UnknownRefund, reconcile.TreasuryMismatch,
}

// DB is the pool of cas_server. *pgxpool.Pool satisfies it.
type DB interface {
	reconcile.DB
	repository.DBTX
}

// Config are the dependencies of the worker.
type Config struct {
	DB         DB
	Connectors *connector.Set
	Clock      limiter.Clock
	// Tick is SYNC_TICK.
	Tick   time.Duration
	Logger *slog.Logger
	// Metrics receives reconciliation_mismatches; nil keeps it unregistered.
	Metrics prometheus.Registerer
}

// Worker reconciles the EVM sources. Run and Pass are not for concurrent use: the engine runs one companion.
type Worker struct {
	cfg        Config
	mismatches *prometheus.GaugeVec
	// notIndexed holds the sources whose treasury was found never indexed and logged once.
	notIndexed map[string]bool
	// lastError holds, per source, the text of the error logged last: the same text is not logged again until the
	// source succeeds. The key "" is an error of no source: the list of sources or the gauge.
	lastError map[string]string
}

// New returns the worker and registers its gauge.
func New(cfg Config) *Worker {
	w := &Worker{
		cfg: cfg,
		mismatches: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "reconciliation_mismatches",
			Help: "Mismatches of the newest reconciliation run of each tenant on the source, summed by type.",
		}, []string{"source", "type"}),
		notIndexed: map[string]bool{},
		lastError:  map[string]string{},
	}
	if cfg.Metrics != nil {
		cfg.Metrics.MustRegister(w.mismatches)
	}
	return w
}

// Run is the companion of the engine: a pass now and then every Tick, until ctx ends. When it ends, the lock lost
// or the process stopping, the gauge is reset: an instance without the lock exposes no series.
func (w *Worker) Run(ctx context.Context) {
	defer w.mismatches.Reset()
	for {
		w.Pass(ctx)
		if err := w.cfg.Clock.Wait(ctx, w.cfg.Clock.Now().Add(w.cfg.Tick)); err != nil {
			return
		}
	}
}

// Pass reconciles every available EVM source with a treasury connection whose logs cursor moved since the newest
// run of the source, or that has no run yet, then sets the gauge from the stored runs. A failure of a source is
// logged and retried at the next pass; it does not stop the other sources.
func (w *Worker) Pass(ctx context.Context) {
	q := repository.New(w.cfg.DB)
	rows, err := q.ListSources(ctx)
	if err != nil {
		w.failed(ctx, "", fmt.Errorf("list the sources: %w", err))
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		src := connector.Source{Code: row.Code, Kind: row.Kind, Enabled: row.Enabled, Config: row.Config}
		if src.Kind != connector.KindEVM || !w.cfg.Connectors.Available(src) {
			continue
		}
		treasury, ok := treasuryOf(row.Config)
		if !ok {
			continue // no treasury connection: the connector logs it at its start (S3 D-23)
		}
		due, err := w.due(ctx, q, row.ID, treasury)
		if err != nil {
			w.failed(ctx, row.Code, err)
			continue
		}
		if due {
			w.reconcile(ctx, row.Code)
		}
	}
	w.refreshGauge(ctx, q)
}

// treasuryOf returns treasury_connection of sources.config when it is set.
func treasuryOf(config []byte) (string, bool) {
	var c map[string]json.RawMessage
	if json.Unmarshal(config, &c) != nil {
		return "", false
	}
	var ref string
	if raw, ok := c[registry.KeyTreasuryConnection]; !ok || json.Unmarshal(raw, &ref) != nil || ref == "" {
		return "", false
	}
	return ref, true
}

// due reports whether next_block − 1 of the treasury logs cursor differs from to_block of the newest run of the
// source, or the source has no run. A treasury that reconcile.Source refuses (a malformed reference, no cursor, no
// next_block) is due, so that its error is reported.
func (w *Worker) due(ctx context.Context, q *repository.Queries, sourceID int16, treasury string) (bool, error) {
	id, err := uuid.Parse(treasury)
	if err != nil {
		return true, nil
	}
	raw, err := q.GetTreasuryLogsCursor(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the logs cursor of the treasury: %w", err)
	}
	var cur struct {
		NextBlock *uint64 `json:"next_block"`
	}
	if json.Unmarshal(raw, &cur) != nil || cur.NextBlock == nil || *cur.NextBlock == 0 || *cur.NextBlock > 1<<63-1 {
		return true, nil
	}
	last, err := q.GetNewestRunToBlock(ctx, sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the newest run of the source: %w", err)
	}
	return int64(*cur.NextBlock-1) != last, nil
}

func (w *Worker) reconcile(ctx context.Context, code string) {
	runs, err := reconcile.Source(ctx, w.cfg.DB, code)
	switch {
	case errors.Is(err, reconcile.ErrNoTreasury):
		return
	case errors.Is(err, reconcile.ErrTreasuryNotIndexed):
		if !w.notIndexed[code] {
			w.notIndexed[code] = true
			w.cfg.Logger.WarnContext(ctx, "reconciliation waits: the treasury connection has no stored logs yet", "source", code)
		}
		return
	case err != nil:
		w.failed(ctx, code, err)
		return
	}
	delete(w.notIndexed, code)
	delete(w.lastError, code)
	for _, r := range runs {
		w.cfg.Logger.InfoContext(ctx, "reconciliation run stored", "source", code, "tenant", r.Tenant,
			"run_id", r.ID.String(), "mismatches", Summary(r.Mismatches))
	}
}

// failed logs one ERROR line for a source and an error text, unless the context ended: the same text again only
// after a success of the source.
func (w *Worker) failed(ctx context.Context, code string, err error) {
	if ctx.Err() != nil {
		return
	}
	if text := err.Error(); w.lastError[code] != text {
		w.lastError[code] = text
		w.cfg.Logger.ErrorContext(ctx, "reconciliation failed; retried at the next tick", "source", code, "error", err)
	}
}

// refreshGauge sets reconciliation_mismatches from the newest run of each tenant on each source: every type of
// every source with a run, 0 when the newest runs hold none, so runs stored by casctl count too.
func (w *Worker) refreshGauge(ctx context.Context, q *repository.Queries) {
	rows, err := q.ListNewestRunMismatches(ctx)
	if err != nil {
		w.failed(ctx, "", fmt.Errorf("read the newest runs for reconciliation_mismatches: %w", err))
		return
	}
	delete(w.lastError, "") // the sources were listed and the runs read
	counts := map[string]map[string]int{}
	for _, r := range rows {
		bySource := counts[r.SourceCode]
		if bySource == nil {
			bySource = map[string]int{}
			for _, t := range MismatchTypes {
				bySource[t] = 0
			}
			counts[r.SourceCode] = bySource
		}
		var ms []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(r.Mismatches, &ms); err != nil {
			w.failed(ctx, r.SourceCode, errors.New("a stored run holds malformed mismatches"))
			continue
		}
		for _, m := range ms {
			bySource[m.Type]++
		}
	}
	for source, byType := range counts {
		for t, n := range byType {
			w.mismatches.WithLabelValues(source, t).Set(float64(n))
		}
	}
}

// Summary is the mismatch counts of a run by type, "MISSING_DEBIT 1, UNKNOWN_DEBIT 2", or "no mismatches".
func Summary(ms []reconcile.Mismatch) string {
	counts := reconcile.Counts(ms)
	if len(counts) == 0 {
		return "no mismatches"
	}
	parts := make([]string, len(counts))
	for i, c := range counts {
		parts[i] = fmt.Sprintf("%s %d", c.Type, c.Count)
	}
	return strings.Join(parts, ", ")
}
