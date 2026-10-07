// Package engine runs the streams of connections (SRS — Core UC-102, ADR-6): the engine lock, a pass every
// SYNC_TICK, a pool of SYNC_WORKERS workers, the periodic key check, the cursors of streams declared later,
// the run of a ledger or balance stream, stream health and the status of the connection. The engine knows
// the connector contract only: it imports no connector implementation and names no source.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/ledger"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Values of SRS — Core §2.3.1, §2.4.
const (
	statusActive             = "ACTIVE"
	statusDegraded           = "DEGRADED"
	statusCredentialsInvalid = "CREDENTIALS_INVALID"

	actionDegraded           = "CONNECTION_DEGRADED"
	actionRecovered          = "CONNECTION_RECOVERED"
	actionCredentialsInvalid = "CREDENTIALS_INVALID"

	// FamilyBalances is the family of the balance stream: a snapshot instead of pages.
	FamilyBalances = "balances"
	// familyUnknown reports a failed run whose stream is not declared, such as when the declared streams
	// cannot be read.
	familyUnknown = "unknown"

	reasonKeyRejected    = "key_rejected"
	reasonKeyNotReadOnly = "key_not_read_only"

	maxLastError  = 500
	redacted      = "[redacted]"
	gaugeInterval = 15 * time.Second
)

// Reporter receives what the engine reports. Tests record it; server exports it as Prometheus metrics.
type Reporter interface {
	// RunFinished: a run of a stream of a family of a source ended in success or failure.
	RunFinished(source, family string, success bool)
	EntriesInserted(source string, n int)
	DuplicatesSkipped(source string, n int)
	// UnmappedAsset: an entry or balance was stored under its native code: no alias (EC-109).
	UnmappedAsset(source, nativeAsset string)
	// BudgetWaited: a reservation in the limiter of a source waited d. The limiter set reports it.
	BudgetWaited(source string, d time.Duration)
	// RateLimited: the source answered with a rate limit.
	RateLimited(source string)
	// Connections: the number of connections by status, read by the engine that holds the lock.
	Connections(byStatus map[string]int)
	// Staleness: per available source, now minus the oldest last_success_at of the streams the engine runs.
	Staleness(bySource map[string]time.Duration)
	// LedgerGap: the gap of the balance checkpoint of a connection and asset, a plain decimal, after the commit of its
	// page (S3 D-19).
	LedgerGap(source, connection, asset, gap string)
}

// Config are the settings of SRS — Core §3.1.
type Config struct {
	MaxPagesPerRun   int
	FailureThreshold int
	BackoffInitial   time.Duration
	BackoffMax       time.Duration
	Workers          int
	Tick             time.Duration
	LockRetry        time.Duration
	KeyCheckInterval time.Duration
}

// DB is what the engine needs from the database: the pool of cas_server. *pgxpool.Pool satisfies it.
type DB interface {
	repository.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Deps are the dependencies of the engine.
type Deps struct {
	DB         DB
	Vault      vault.Vault
	Connectors *connector.Set
	// Limiters is the one limiter set of the process, shared with the connection registry (FR-110).
	Limiters *limiter.Set
	// Locker takes the engine lock; Run needs it, RunPass and RunConnection do not.
	Locker   Locker
	Clock    limiter.Clock
	Reporter Reporter
	Logger   *slog.Logger
}

// Engine runs streams. It is safe for concurrent use.
type Engine struct {
	cfg        Config
	db         DB
	vault      vault.Vault
	connectors *connector.Set
	limiters   *limiter.Set
	locker     Locker
	clock      limiter.Clock
	reporter   Reporter
	logger     *slog.Logger
	ledger     *ledger.Writer

	mu        sync.Mutex
	inFlight  map[uuid.UUID]bool
	keyChecks map[uuid.UUID]keyBackoff
}

// keyBackoff is the in-memory count of failed key checks of a connection (EC-118): a new engine value
// checks at once.
type keyBackoff struct {
	attempts int
	next     time.Time
}

// New returns an engine.
func New(cfg Config, deps Deps) *Engine {
	return &Engine{
		cfg: cfg, db: deps.DB, vault: deps.Vault, connectors: deps.Connectors, limiters: deps.Limiters,
		locker: deps.Locker, clock: deps.Clock, reporter: deps.Reporter, logger: deps.Logger,
		ledger:    ledger.NewWriter(deps.DB, deps.Clock.Now),
		inFlight:  map[uuid.UUID]bool{},
		keyChecks: map[uuid.UUID]keyBackoff{},
	}
}

// Run takes the engine lock every SYNC_LOCK_RETRY and, while it holds it, creates the cursors of streams
// declared later, then runs a pass every SYNC_TICK on SYNC_WORKERS workers. A lost lock stops the runs and
// starts over. When ctx ends, no new run starts, the running ones stop at their next step, the lock is
// released and Run returns after them.
func (e *Engine) Run(ctx context.Context) error {
	for {
		lease, err := e.locker.TryLock(ctx)
		if err != nil && ctx.Err() == nil {
			e.logger.WarnContext(ctx, "cannot try the engine lock", "error", err)
		}
		if lease != nil {
			e.logger.InfoContext(ctx, "engine lock taken")
			e.runLocked(ctx, lease)
			lease.Release()
			e.logger.InfoContext(ctx, "engine lock released")
		}
		if ctx.Err() != nil {
			return nil
		}
		if lease == nil {
			if err := e.clock.Wait(ctx, e.clock.Now().Add(e.cfg.LockRetry)); err != nil {
				return nil
			}
		}
	}
}

func (e *Engine) runLocked(ctx context.Context, lease Lease) {
	runCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()

	e.CreateMissingCursors(runCtx)
	var lastGauges time.Time
	for {
		if err := lease.Held(runCtx); err != nil {
			if runCtx.Err() == nil {
				e.logger.ErrorContext(ctx, "engine lock lost: the runs stop", "error", err)
			}
			return
		}
		now := e.clock.Now()
		if lastGauges.IsZero() || now.Sub(lastGauges) >= gaugeInterval {
			e.RefreshGauges(runCtx)
			lastGauges = now
		}
		e.dispatch(runCtx, &workers)
		if err := e.clock.Wait(runCtx, now.Add(e.cfg.Tick)); err != nil {
			return
		}
	}
}

// dispatch gives the connections with work to free workers. A connection already in a worker is skipped:
// it never runs twice at once, also when a pass starts while the previous one still runs.
func (e *Engine) dispatch(ctx context.Context, workers *sync.WaitGroup) {
	works, err := e.dueWork(ctx, e.clock.Now(), nil)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.ErrorContext(ctx, "cannot list the due work", "error", err)
		}
		return
	}
	for _, w := range works {
		e.mu.Lock()
		if e.inFlight[w.connectionID] || len(e.inFlight) >= e.cfg.Workers {
			e.mu.Unlock()
			continue
		}
		e.inFlight[w.connectionID] = true
		e.mu.Unlock()
		workers.Go(func() {
			defer func() {
				e.mu.Lock()
				delete(e.inFlight, w.connectionID)
				e.mu.Unlock()
			}()
			e.runWork(ctx, w)
		})
	}
}

// RunPass runs every due work at the time of the clock one connection after another, without workers and
// without the engine lock. Connections are taken in the order of their earliest due work.
func (e *Engine) RunPass(ctx context.Context) error {
	works, err := e.dueWork(ctx, e.clock.Now(), nil)
	if err != nil {
		return err
	}
	for _, w := range works {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.runWork(ctx, w)
	}
	return ctx.Err()
}

// RunConnection runs the due work of one connection at the time of the clock.
func (e *Engine) RunConnection(ctx context.Context, id uuid.UUID) error {
	works, err := e.dueWork(ctx, e.clock.Now(), &id)
	if err != nil {
		return err
	}
	for _, w := range works {
		e.runWork(ctx, w)
	}
	return ctx.Err()
}

// work is what is due for one connection: a key check first, then its streams in the order of next_run_at.
type work struct {
	connectionID uuid.UUID
	keyCheck     bool
	streams      []string
	dueAt        time.Time
}

func (e *Engine) dueWork(ctx context.Context, now time.Time, id *uuid.UUID) ([]work, error) {
	q := repository.New(e.db)
	streams, err := q.ListDueStreams(ctx, repository.ListDueStreamsParams{Now: now, ConnectionID: id})
	if err != nil {
		return nil, fmt.Errorf("list the due streams: %w", err)
	}
	checks, err := q.ListDueKeyChecks(ctx, repository.ListDueKeyChecksParams{
		DueBefore: ptr(now.Add(-e.cfg.KeyCheckInterval)), ConnectionID: id,
	})
	if err != nil {
		return nil, fmt.Errorf("list the due key checks: %w", err)
	}

	var works []*work
	byID := map[uuid.UUID]*work{}
	get := func(id uuid.UUID, dueAt time.Time) *work {
		w, ok := byID[id]
		if !ok {
			w = &work{connectionID: id, dueAt: dueAt}
			byID[id] = w
			works = append(works, w)
		}
		if dueAt.Before(w.dueAt) {
			w.dueAt = dueAt
		}
		return w
	}
	e.mu.Lock()
	for _, c := range checks {
		if b, ok := e.keyChecks[c.ID]; ok && now.Before(b.next) {
			continue
		}
		var dueAt time.Time
		if c.PermissionsCheckedAt != nil {
			dueAt = c.PermissionsCheckedAt.Add(e.cfg.KeyCheckInterval)
		}
		get(c.ID, dueAt).keyCheck = true
	}
	e.mu.Unlock()
	for _, s := range streams {
		w := get(s.ConnectionID, s.NextRunAt)
		w.streams = append(w.streams, s.Stream)
	}
	sort.SliceStable(works, func(i, j int) bool { return works[i].dueAt.Before(works[j].dueAt) })
	out := make([]work, len(works))
	for i, w := range works {
		out[i] = *w
	}
	return out, nil
}

// run is one run of the due work of a connection.
type run struct {
	id      uuid.UUID
	source  connector.Source
	conn    connector.Connector
	call    connector.Connection
	secrets []string
}

// outcome of a step: whether the connection goes on with its next step.
type outcome int

const (
	next outcome = iota
	stop
)

func (e *Engine) runWork(ctx context.Context, w work) {
	row, err := repository.New(e.db).GetSyncConnection(ctx, w.connectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			e.logger.ErrorContext(ctx, "cannot read a connection to sync", "connection_id", w.connectionID, "error", err)
		}
		return
	}
	aliases, err := repository.New(e.db).AliasesOfSource(ctx, row.SourceCode)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.ErrorContext(ctx, "cannot read the aliases of a source", "source", row.SourceCode, "error", err)
		}
		return
	}
	src := connector.Source{Code: row.SourceCode, Kind: row.SourceKind, Enabled: row.SourceEnabled, Config: row.SourceConfig,
		Aliases: aliases}
	// Not run and no failure counted while the source is not available (UC-102 preconditions).
	if !e.connectors.Available(src) {
		return
	}
	conn, _ := e.connectors.For(src)
	r := &run{id: row.ID, source: src, conn: conn,
		call: connector.Connection{ID: row.ID.String(), Source: src, Account: row.ExternalAccount}}

	var prepErr error
	if row.CredentialsEnc != nil {
		prepErr = e.decryptKey(r, row)
	}
	if prepErr == nil {
		r.call.Limiter, prepErr = e.limiters.For(src, conn)
	}
	if w.keyCheck {
		if prepErr != nil {
			e.keyCheckFailed(ctx, r, prepErr)
		} else if e.checkKey(ctx, r) == stop {
			return
		}
	}
	if len(w.streams) == 0 {
		return
	}

	declared, err := conn.Streams(ctx, src, connector.AccountInfo{Identity: row.ExternalAccount})
	if err != nil && prepErr == nil {
		prepErr = fmt.Errorf("declare the streams: %w", err)
	}
	byName := make(map[string]connector.Stream, len(declared))
	for _, s := range declared {
		byName[s.Name] = s
	}
	for _, name := range w.streams {
		if ctx.Err() != nil {
			return
		}
		s, ok := byName[name]
		if !ok && prepErr == nil {
			// A cursor of a stream the connector no longer declares is kept and not run.
			continue
		}
		if prepErr != nil {
			family := s.Family
			if family == "" {
				family = familyUnknown
			}
			if e.recordFailure(ctx, r, name, family, prepErr) == stop {
				return
			}
			continue
		}
		if e.runStream(ctx, r, s) == stop {
			return
		}
	}
}

// decryptKey decrypts the key of the connection for this run only.
func (e *Engine) decryptKey(r *run, row repository.GetSyncConnectionRow) error {
	if row.KekVersion == nil {
		return errors.New("the connection has a key without a kek version")
	}
	plaintext, err := e.vault.Decrypt([16]byte(row.ID), row.CredentialsEnc, *row.KekVersion)
	if err != nil {
		return errors.New("the key of the connection cannot be decrypted")
	}
	defer clear(plaintext)
	var k struct {
		APIKey    string `json:"api_key"`
		APISecret string `json:"api_secret"`
	}
	if err := json.Unmarshal(plaintext, &k); err != nil {
		return errors.New("the key of the connection cannot be decrypted")
	}
	r.call.Key = &connector.ExchangeKey{APIKey: vault.NewSecret(k.APIKey), APISecret: vault.NewSecret(k.APISecret)}
	r.secrets = []string{k.APIKey, k.APISecret}
	return nil
}

// checkKey is the periodic key check (FR-119): the account check with the decrypted key, through the limiter.
func (e *Engine) checkKey(ctx context.Context, r *run) outcome {
	info, err := r.conn.CheckAccount(ctx, r.source, connector.Credentials{ExchangeKey: r.call.Key}, r.call.Limiter)
	if ctx.Err() != nil {
		return stop
	}
	var notReadOnly *connector.KeyNotReadOnlyError
	switch {
	case errors.Is(err, connector.ErrKeyRejected):
		return e.invalidate(ctx, r, invalidation{reason: reasonKeyRejected})
	case errors.As(err, &notReadOnly):
		return e.invalidate(ctx, r, invalidation{reason: reasonKeyNotReadOnly, permissions: notReadOnly.Permissions})
	case err != nil:
		e.keyCheckFailed(ctx, r, err)
		return next
	}
	if r.conn.Capabilities().PermissionsReadable {
		var beyond []string
		for _, p := range info.Permissions {
			if p != connector.PermissionRead && !slices.Contains(beyond, p) {
				beyond = append(beyond, p)
			}
		}
		if len(beyond) > 0 {
			return e.invalidate(ctx, r, invalidation{reason: reasonKeyNotReadOnly, permissions: beyond})
		}
		if len(info.Permissions) == 0 {
			e.keyCheckFailed(ctx, r, errors.New("the connector reads permissions but returned none"))
			return next
		}
	}
	// Read-only, or not readable at the source (UNVERIFIED stays UNVERIFIED): only the time moves.
	if _, err := repository.New(e.db).SetPermissionsChecked(ctx, repository.SetPermissionsCheckedParams{
		ID: r.id, CheckedAt: ptr(e.now()),
	}); err != nil {
		e.keyCheckFailed(ctx, r, fmt.Errorf("store the key check: %w", err))
		return next
	}
	e.mu.Lock()
	delete(e.keyChecks, r.id)
	e.mu.Unlock()
	return next
}

// keyCheckFailed keeps the status and permissions_checked_at and sets the next attempt by the backoff of
// UC-102 step 8, not before the end of a rate-limit pause (EC-118). The count lives in memory.
func (e *Engine) keyCheckFailed(ctx context.Context, r *run, err error) {
	now := e.now()
	var rateLimit *connector.RateLimitError
	isRateLimit := errors.As(err, &rateLimit)
	if isRateLimit {
		e.reporter.RateLimited(r.source.Code)
	}
	e.mu.Lock()
	b := e.keyChecks[r.id]
	b.attempts++
	b.next = now.Add(e.backoff(b.attempts))
	if isRateLimit {
		if end := now.Add(rateLimit.Pause); end.After(b.next) {
			b.next = end
		}
	}
	e.keyChecks[r.id] = b
	e.mu.Unlock()
	e.logger.WarnContext(ctx, "key check failed", "source", r.source.Code, "connection_id", r.id,
		"attempts", b.attempts, "error", e.sanitize(r, err.Error()))
}

// invalidation is a key that the source rejected or that is no longer read-only (EC-108, EC-116).
type invalidation struct {
	reason      string
	permissions []string
}

func (i invalidation) details(stream string) map[string]any {
	d := map[string]any{"status": statusCredentialsInvalid, "reason": i.reason}
	if stream != "" {
		d["stream"] = stream
	}
	if i.reason == reasonKeyNotReadOnly {
		d["permissions"] = i.permissions
	}
	return d
}

// invalidate sets CREDENTIALS_INVALID with its audit row after a key check. The streams are not run.
func (e *Engine) invalidate(ctx context.Context, r *run, inv invalidation) outcome {
	err := pgx.BeginFunc(ctx, e.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		conn, err := q.LockConnectionStatus(ctx, r.id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if conn.Status == statusCredentialsInvalid {
			return nil
		}
		return setStatus(ctx, q, r.id, conn.TenantID, statusCredentialsInvalid, actionCredentialsInvalid, inv.details(""))
	})
	if err != nil {
		e.logger.ErrorContext(ctx, "cannot store the result of a key check", "connection_id", r.id, "error", err)
	}
	e.mu.Lock()
	delete(e.keyChecks, r.id)
	e.mu.Unlock()
	e.logger.WarnContext(ctx, "key check: the connection is stopped", "source", r.source.Code,
		"connection_id", r.id, "reason", inv.reason)
	return stop
}

func (e *Engine) runStream(ctx context.Context, r *run, s connector.Stream) outcome {
	if s.Family == FamilyBalances {
		snap, err := r.conn.FetchSnapshot(ctx, r.call)
		if err != nil {
			return e.recordFailure(ctx, r, s.Name, s.Family, err)
		}
		res, err := e.ledger.WriteSnapshot(ctx, r.id, snap)
		if err != nil {
			return e.recordFailure(ctx, r, s.Name, s.Family, err)
		}
		if res.Gone {
			return stop
		}
		e.reportUnmapped(ctx, r, res.Unmapped)
		return e.recordSuccess(ctx, r, s, s.Interval)
	}

	cur, err := repository.New(e.db).GetStreamCursor(ctx, repository.GetStreamCursorParams{ConnectionID: r.id, Stream: s.Name})
	if errors.Is(err, pgx.ErrNoRows) {
		return stop
	}
	if err != nil {
		return e.recordFailure(ctx, r, s.Name, s.Family, fmt.Errorf("read the cursor: %w", err))
	}
	mode, cursor := connector.Mode(cur.Mode), json.RawMessage(cur.Cursor)
	for pages := 1; ; pages++ {
		page, err := r.conn.FetchPage(ctx, r.call, s.Name, mode, cursor)
		if err != nil {
			return e.recordFailure(ctx, r, s.Name, s.Family, err)
		}
		res, err := e.ledger.WritePage(ctx, r.id, s.Name, s.Family, page)
		if err != nil {
			return e.recordFailure(ctx, r, s.Name, s.Family, err)
		}
		if res.Gone {
			return stop
		}
		if res.Inserted > 0 {
			e.reporter.EntriesInserted(r.source.Code, res.Inserted)
		}
		if res.Skipped > 0 {
			e.reporter.DuplicatesSkipped(r.source.Code, res.Skipped)
		}
		e.reportUnmapped(ctx, r, res.Unmapped)
		for _, g := range res.Gaps {
			e.reporter.LedgerGap(r.source.Code, r.id.String(), g.Asset, g.Gap)
		}
		mode, cursor = page.Mode, page.Cursor
		switch {
		case !page.More:
			return e.recordSuccess(ctx, r, s, s.Interval)
		case pages >= e.cfg.MaxPagesPerRun:
			// At the page limit with more pages: a success, due at once behind the streams already due.
			return e.recordSuccess(ctx, r, s, 0)
		case ctx.Err() != nil:
			return stop
		}
	}
}

func (e *Engine) reportUnmapped(ctx context.Context, r *run, natives []string) {
	for _, native := range natives {
		e.reporter.UnmappedAsset(r.source.Code, native)
		e.logger.WarnContext(ctx, "asset without alias: stored under its native code",
			"source", r.source.Code, "native_asset", native, "connection_id", r.id)
	}
}

// recordSuccess stores the health of a successful run with the status of the connection.
func (e *Engine) recordSuccess(ctx context.Context, r *run, s connector.Stream, interval time.Duration) outcome {
	now := e.now()
	out, err := e.health(ctx, r, s.Name, func(q *repository.Queries) (int64, error) {
		return q.RecordStreamSuccess(ctx, repository.RecordStreamSuccessParams{
			ConnectionID: r.id, Stream: s.Name, LastSuccessAt: &now, NextRunAt: now.Add(interval),
		})
	}, nil)
	if err != nil {
		e.logger.ErrorContext(ctx, "cannot store the health of a run", "connection_id", r.id, "stream", s.Name, "error", err)
		return stop
	}
	e.reporter.RunFinished(r.source.Code, s.Family, true)
	e.logger.DebugContext(ctx, "sync run finished", "source", r.source.Code, "stream", s.Name,
		"connection_id", r.id, "result", "success")
	return out
}

// recordFailure stores a failed run: last_error without secrets, the failure counter, the backoff and,
// for a rejected or no longer read-only key, CREDENTIALS_INVALID (EC-108).
func (e *Engine) recordFailure(ctx context.Context, r *run, stream, family string, runErr error) outcome {
	if ctx.Err() != nil {
		// Stopped from outside: no health is stored; the next run continues from the cursor.
		return stop
	}
	message := e.sanitize(r, runErr.Error())
	var rateLimit *connector.RateLimitError
	var notReadOnly *connector.KeyNotReadOnlyError
	isRateLimit := errors.As(runErr, &rateLimit)
	var inv *invalidation
	switch {
	case errors.Is(runErr, connector.ErrKeyRejected):
		inv = &invalidation{reason: reasonKeyRejected}
	case errors.As(runErr, &notReadOnly):
		inv = &invalidation{reason: reasonKeyNotReadOnly, permissions: notReadOnly.Permissions}
	}
	if isRateLimit {
		e.reporter.RateLimited(r.source.Code)
	}

	now := e.now()
	out, err := e.health(ctx, r, stream, func(q *repository.Queries) (int64, error) {
		failures, err := q.LockStreamFailures(ctx, repository.LockStreamFailuresParams{ConnectionID: r.id, Stream: stream})
		if err != nil {
			return 0, err
		}
		failures++
		nextRun := now.Add(e.backoff(int(failures)))
		if isRateLimit {
			if end := now.Add(rateLimit.Pause); end.After(nextRun) {
				nextRun = end
			}
		}
		return q.RecordStreamFailure(ctx, repository.RecordStreamFailureParams{
			ConnectionID: r.id, Stream: stream, LastError: &message, ConsecutiveFailures: failures, NextRunAt: nextRun,
		})
	}, inv)
	if err != nil {
		e.logger.ErrorContext(ctx, "cannot store the health of a run", "connection_id", r.id, "stream", stream, "error", err)
		return stop
	}
	e.reporter.RunFinished(r.source.Code, family, false)
	e.logger.WarnContext(ctx, "sync run failed", "source", r.source.Code, "stream", stream, "connection_id", r.id,
		"error", message)
	if inv != nil {
		return stop
	}
	return out
}

// health runs update and the status rule of the connection in one transaction. Locks: the connection row,
// then the cursor row. A connection that is gone stops the run.
func (e *Engine) health(ctx context.Context, r *run, stream string, update func(*repository.Queries) (int64, error), inv *invalidation) (outcome, error) {
	out := next
	err := pgx.BeginFunc(ctx, e.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		conn, err := q.LockConnectionStatus(ctx, r.id)
		if errors.Is(err, pgx.ErrNoRows) {
			out = stop
			return nil
		}
		if err != nil {
			return err
		}
		n, err := update(q)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && n == 0) {
			out = stop
			return nil
		}
		if err != nil {
			return err
		}
		if conn.Status == statusCredentialsInvalid {
			// Final: the rule below never changes it.
			return nil
		}
		if inv != nil {
			return setStatus(ctx, q, r.id, conn.TenantID, statusCredentialsInvalid, actionCredentialsInvalid, inv.details(stream))
		}
		over, err := q.AnyStreamOverThreshold(ctx, repository.AnyStreamOverThresholdParams{
			ConnectionID: r.id, Threshold: int32(e.cfg.FailureThreshold),
		})
		if err != nil {
			return err
		}
		switch {
		case over && conn.Status == statusActive:
			return setStatus(ctx, q, r.id, conn.TenantID, statusDegraded, actionDegraded,
				map[string]any{"status": statusDegraded, "stream": stream})
		case !over && conn.Status == statusDegraded:
			return setStatus(ctx, q, r.id, conn.TenantID, statusActive, actionRecovered,
				map[string]any{"status": statusActive, "stream": stream})
		}
		return nil
	})
	return out, err
}

// setStatus changes the status of a connection and writes its audit row, with no credential: the engine acts.
func setStatus(ctx context.Context, q *repository.Queries, id, tenantID uuid.UUID, status, action string, details map[string]any) error {
	if err := q.SetConnectionStatus(ctx, repository.SetConnectionStatusParams{ID: id, Status: status}); err != nil {
		return err
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return q.InsertAuditLog(ctx, repository.InsertAuditLogParams{
		TenantID: tenantID, Action: action, ObjectID: id.String(), Details: raw,
	})
}

// CreateMissingCursors gives every declared stream without a cursor its cursor, due at once (FR-122,
// EC-119), for the connections ACTIVE or DEGRADED of an available source. Existing cursors are not changed.
func (e *Engine) CreateMissingCursors(ctx context.Context) {
	q := repository.New(e.db)
	rows, err := q.ListConnectionsToSync(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.ErrorContext(ctx, "cannot list the connections for missing cursors", "error", err)
		}
		return
	}
	aliases, err := q.AliasesBySource(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.ErrorContext(ctx, "cannot read the aliases for missing cursors", "error", err)
		}
		return
	}
	now := e.now()
	for _, row := range rows {
		src := connector.Source{Code: row.SourceCode, Kind: row.SourceKind, Enabled: row.SourceEnabled, Config: row.SourceConfig,
			Aliases: aliases[row.SourceCode]}
		if !e.connectors.Available(src) {
			continue
		}
		conn, _ := e.connectors.For(src)
		declared, err := conn.Streams(ctx, src, connector.AccountInfo{Identity: row.ExternalAccount})
		if err != nil {
			e.logger.WarnContext(ctx, "cannot read the declared streams", "connection_id", row.ID, "error", err)
			continue
		}
		existing, err := q.ListCursorStreams(ctx, row.ID)
		if err != nil {
			e.logger.ErrorContext(ctx, "cannot read the cursors", "connection_id", row.ID, "error", err)
			continue
		}
		for _, s := range declared {
			if slices.Contains(existing, s.Name) {
				continue
			}
			cursor := s.FirstCursor
			if len(cursor) == 0 {
				cursor = json.RawMessage(`{}`)
			}
			n, err := q.InsertMissingSyncCursor(ctx, repository.InsertMissingSyncCursorParams{
				ConnectionID: row.ID, Stream: s.Name, Mode: string(s.FirstMode), Cursor: cursor, NextRunAt: now,
			})
			if err != nil {
				e.logger.ErrorContext(ctx, "cannot create a cursor", "connection_id", row.ID, "stream", s.Name, "error", err)
				continue
			}
			if n == 1 {
				e.logger.InfoContext(ctx, "cursor of a stream declared later created", "connection_id", row.ID, "stream", s.Name)
			}
		}
	}
}

// RefreshGauges reads connections by status and the staleness of each available source.
func (e *Engine) RefreshGauges(ctx context.Context) {
	q := repository.New(e.db)
	counts, err := q.CountConnectionsByStatus(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.ErrorContext(ctx, "cannot count the connections", "error", err)
		}
		return
	}
	byStatus := map[string]int{statusActive: 0, statusDegraded: 0, statusCredentialsInvalid: 0}
	for _, c := range counts {
		byStatus[c.Status] = int(c.N)
	}
	e.reporter.Connections(byStatus)

	now := e.now()
	rows, err := q.ListSourceStaleness(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.ErrorContext(ctx, "cannot read the staleness", "error", err)
		}
		return
	}
	aliases, err := q.AliasesBySource(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.ErrorContext(ctx, "cannot read the aliases", "error", err)
		}
		return
	}
	bySource := map[string]time.Duration{}
	for _, row := range rows {
		src := connector.Source{Code: row.Code, Kind: row.Kind, Enabled: row.Enabled, Config: row.Config, Aliases: aliases[row.Code]}
		if !e.connectors.Available(src) {
			continue
		}
		if row.Succeeded == 0 {
			bySource[row.Code] = 0
			continue
		}
		bySource[row.Code] = now.Sub(row.Oldest)
	}
	e.reporter.Staleness(bySource)
}

// backoff is SYNC_BACKOFF_INITIAL doubled per failure, at most SYNC_BACKOFF_MAX.
func (e *Engine) backoff(failures int) time.Duration {
	d := e.cfg.BackoffInitial
	for i := 1; i < failures; i++ {
		if d >= e.cfg.BackoffMax/2 {
			return e.cfg.BackoffMax
		}
		d *= 2
	}
	return min(d, e.cfg.BackoffMax)
}

// sanitize removes every occurrence of the key and the secret, then cuts to 500 characters.
func (e *Engine) sanitize(r *run, s string) string {
	for _, secret := range r.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, redacted)
		}
	}
	if utf8.RuneCountInString(s) > maxLastError {
		s = string([]rune(s)[:maxLastError])
	}
	return s
}

func (e *Engine) now() time.Time {
	return e.clock.Now().UTC().Truncate(time.Microsecond)
}

func ptr[T any](v T) *T { return &v }
