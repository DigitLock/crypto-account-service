// Package engine runs the streams of connections (SRS — Core UC-102, ADR-6): one scheduler pass, the run
// of a ledger or balance stream, stream health and the status of the connection. The worker pool, the
// timer loop and the engine lock come in st7b. The engine knows the connector contract only: it imports
// no connector implementation and names no source.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

	maxLastError = 500
	redacted     = "[redacted]"
)

// Reporter receives what the engine reports. Tests record it; st7b exports it as Prometheus metrics.
type Reporter interface {
	// RunFinished: a run of a stream of a family of a source ended in success or failure.
	RunFinished(source, family string, success bool)
	EntriesInserted(source string, n int)
	DuplicatesSkipped(source string, n int)
	// UnmappedAsset: an entry or balance was stored under its native code: no alias (EC-109).
	UnmappedAsset(source, nativeAsset string)
	// BudgetWaited: a reservation in the limiter of a source waited d.
	BudgetWaited(source string, d time.Duration)
	// RateLimited: the source answered with a rate limit.
	RateLimited(source string)
}

// Config are the settings of SRS — Core §3.1.
type Config struct {
	MaxPagesPerRun   int
	FailureThreshold int
	BackoffInitial   time.Duration
	BackoffMax       time.Duration
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
	Clock      limiter.Clock
	Reporter   Reporter
	Logger     *slog.Logger
}

// Engine runs streams. It is safe for concurrent use: connections may run from several goroutines.
type Engine struct {
	cfg        Config
	db         DB
	vault      vault.Vault
	connectors *connector.Set
	clock      limiter.Clock
	reporter   Reporter
	logger     *slog.Logger
	ledger     *ledger.Writer

	mu       sync.Mutex
	limiters map[string]*limiter.Limiter
}

// New returns an engine.
func New(cfg Config, deps Deps) *Engine {
	return &Engine{
		cfg: cfg, db: deps.DB, vault: deps.Vault, connectors: deps.Connectors, clock: deps.Clock,
		reporter: deps.Reporter, logger: deps.Logger,
		ledger:   ledger.NewWriter(deps.DB, deps.Clock.Now),
		limiters: map[string]*limiter.Limiter{},
	}
}

type due struct {
	connectionID uuid.UUID
	streams      []string
}

// RunPass runs every stream due at the time of the clock (UC-102 step 1). Connections are taken in the
// order of their earliest due stream; the due streams of one connection run one after another in the order
// of next_run_at. It returns the error of ctx when ctx ends.
func (e *Engine) RunPass(ctx context.Context) error {
	groups, err := e.dueStreams(ctx, nil)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.runConnection(ctx, g)
	}
	return ctx.Err()
}

// RunConnection runs the streams of one connection that are due at the time of the clock.
func (e *Engine) RunConnection(ctx context.Context, id uuid.UUID) error {
	groups, err := e.dueStreams(ctx, &id)
	if err != nil {
		return err
	}
	for _, g := range groups {
		e.runConnection(ctx, g)
	}
	return ctx.Err()
}

func (e *Engine) dueStreams(ctx context.Context, id *uuid.UUID) ([]due, error) {
	rows, err := repository.New(e.db).ListDueStreams(ctx, repository.ListDueStreamsParams{Now: e.clock.Now(), ConnectionID: id})
	if err != nil {
		return nil, fmt.Errorf("list the due streams: %w", err)
	}
	var groups []due
	index := map[uuid.UUID]int{}
	for _, r := range rows {
		i, ok := index[r.ConnectionID]
		if !ok {
			i = len(groups)
			index[r.ConnectionID] = i
			groups = append(groups, due{connectionID: r.ConnectionID})
		}
		groups[i].streams = append(groups[i].streams, r.Stream)
	}
	return groups, nil
}

// run is one run of the due streams of a connection.
type run struct {
	id      uuid.UUID
	source  string
	conn    connector.Connector
	call    connector.Connection
	secrets []string
}

// outcome of a stream run: whether the connection goes on with its next stream.
type outcome int

const (
	next outcome = iota
	stop
)

func (e *Engine) runConnection(ctx context.Context, g due) {
	row, err := repository.New(e.db).GetSyncConnection(ctx, g.connectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		e.logger.ErrorContext(ctx, "cannot read a connection to sync", "connection_id", g.connectionID, "error", err)
		return
	}
	src := connector.Source{Code: row.SourceCode, Kind: row.SourceKind, Enabled: row.SourceEnabled, Config: row.SourceConfig}
	// Not run and no failure counted while the source is not available (UC-102 preconditions).
	if !e.connectors.Available(src) {
		return
	}
	conn, _ := e.connectors.For(src)
	r := &run{id: row.ID, source: src.Code, conn: conn,
		call: connector.Connection{ID: row.ID.String(), Source: src, Account: row.ExternalAccount}}

	declared, prepErr := conn.Streams(ctx, src, connector.AccountInfo{Identity: row.ExternalAccount})
	if prepErr == nil && row.CredentialsEnc != nil {
		prepErr = e.decryptKey(r, row)
	}
	if prepErr == nil {
		r.call.Limiter, prepErr = e.limiterFor(src, conn)
	}

	byName := make(map[string]connector.Stream, len(declared))
	for _, s := range declared {
		byName[s.Name] = s
	}
	for _, name := range g.streams {
		if ctx.Err() != nil {
			return
		}
		s, ok := byName[name]
		if !ok && prepErr == nil {
			// A cursor of a stream the connector no longer declares is kept and not run.
			continue
		}
		if prepErr != nil {
			if e.recordFailure(ctx, r, name, s.Family, prepErr) == stop {
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

// limiterFor returns the one limiter of a source, built from the budgets its connector declares.
func (e *Engine) limiterFor(src connector.Source, conn connector.Connector) (*limiter.Limiter, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if l, ok := e.limiters[src.Code]; ok {
		return l, nil
	}
	code := src.Code
	l, err := limiter.New(conn.Budgets(src), e.clock, func(d time.Duration) { e.reporter.BudgetWaited(code, d) })
	if err != nil {
		return nil, err
	}
	e.limiters[src.Code] = l
	return l, nil
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
			e.reporter.EntriesInserted(r.source, res.Inserted)
		}
		if res.Skipped > 0 {
			e.reporter.DuplicatesSkipped(r.source, res.Skipped)
		}
		e.reportUnmapped(ctx, r, res.Unmapped)
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
		e.reporter.UnmappedAsset(r.source, native)
		e.logger.WarnContext(ctx, "asset without alias: stored under its native code",
			"source", r.source, "native_asset", native, "connection_id", r.id)
	}
}

// recordSuccess stores the health of a successful run with the status of the connection.
func (e *Engine) recordSuccess(ctx context.Context, r *run, s connector.Stream, interval time.Duration) outcome {
	now := e.now()
	out, err := e.health(ctx, r, s.Name, func(q *repository.Queries) (int64, error) {
		return q.RecordStreamSuccess(ctx, repository.RecordStreamSuccessParams{
			ConnectionID: r.id, Stream: s.Name, LastSuccessAt: &now, NextRunAt: now.Add(interval),
		})
	}, false)
	if err != nil {
		e.logger.ErrorContext(ctx, "cannot store the health of a run", "connection_id", r.id, "stream", s.Name, "error", err)
		return stop
	}
	e.reporter.RunFinished(r.source, s.Family, true)
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
	invalidKey := errors.Is(runErr, connector.ErrKeyRejected) || errors.As(runErr, &notReadOnly)
	if isRateLimit {
		e.reporter.RateLimited(r.source)
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
	}, invalidKey)
	if err != nil {
		e.logger.ErrorContext(ctx, "cannot store the health of a run", "connection_id", r.id, "stream", stream, "error", err)
		return stop
	}
	e.reporter.RunFinished(r.source, family, false)
	e.logger.WarnContext(ctx, "sync run failed", "source", r.source, "stream", stream, "connection_id", r.id, "error", message)
	if invalidKey {
		return stop
	}
	return out
}

// health runs update and the status rule of the connection in one transaction. Locks: the connection row,
// then the cursor row. A connection that is gone stops the run.
func (e *Engine) health(ctx context.Context, r *run, stream string, update func(*repository.Queries) (int64, error), invalidKey bool) (outcome, error) {
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
		newStatus, action := conn.Status, ""
		if invalidKey {
			newStatus, action = statusCredentialsInvalid, actionCredentialsInvalid
		} else {
			over, err := q.AnyStreamOverThreshold(ctx, repository.AnyStreamOverThresholdParams{
				ConnectionID: r.id, Threshold: int32(e.cfg.FailureThreshold),
			})
			if err != nil {
				return err
			}
			switch {
			case over && conn.Status == statusActive:
				newStatus, action = statusDegraded, actionDegraded
			case !over && conn.Status == statusDegraded:
				newStatus, action = statusActive, actionRecovered
			}
		}
		if action == "" {
			return nil
		}
		if err := q.SetConnectionStatus(ctx, repository.SetConnectionStatusParams{ID: r.id, Status: newStatus}); err != nil {
			return err
		}
		details, err := json.Marshal(map[string]string{"status": newStatus, "stream": stream})
		if err != nil {
			return err
		}
		return q.InsertAuditLog(ctx, repository.InsertAuditLogParams{
			TenantID: conn.TenantID, Action: action, ObjectID: r.id.String(), Details: details,
		})
	})
	return out, err
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
