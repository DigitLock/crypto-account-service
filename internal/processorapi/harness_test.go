package processorapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/api/openapi"
	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/crs"
	"github.com/DigitLock/crypto-account-service/internal/crs/crstest"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/processorapi"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Harness of phase 4 of docs/test-plan-s2.md: the processor API with the real decision engine on the test
// database (role cas_card_auth), Anvil with the deployed contracts, a fake CRS and a fake Debit step. Every
// response of every request is validated against api/openapi (S2-T203).

var ctx = context.Background()

// usdc returns base units of whole and fractional USDC given as a decimal string with up to 6 places.
func usdc(t testing.TB, s string) *big.Int {
	t.Helper()
	n, err := decision.TokenAmount(s, "", 0, testchain.TokenDecimals)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// options change the harness.
type options struct {
	chain         *testchain.Chain // nil: a new chain
	rpcURL        string           // primary endpoint; empty: the chain
	fallbackURL   string
	fallbackAfter int
	probe         time.Duration
	readTimeout   time.Duration
	deadline      time.Duration
	noCRS         bool // CRS_ADDRESS unset
	crsAddr       string
	db            func(*pgxpool.Pool) decision.DB
	readsDB       func(*pgxpool.Pool) decision.DB // the status query and the credentials
	debit         *fakeDebit
	bufferBPS     *int
}

// env is a running processor API with its tenant A, card_A and wallet.
type env struct {
	owner    *pgxpool.Pool
	cardAuth *pgxpool.Pool
	chain    *testchain.Chain
	crs      *crstest.Server
	debit    *fakeDebit
	engine   *spyEngine
	clock    *fakeClock
	metrics  *prometheus.Registry
	srv      *httptest.Server
	router   routers.Router
	logs     *syncBuffer
	reader   *chain.Reader

	tenantA uuid.UUID
	pairA   registry.IssuedPair
	wallet  common.Address
	cardA   uuid.UUID
}

// newEnv sets up the common preconditions of docs/test-plan-s2.md §2: tenant A with a Basic pair; a wallet with
// 100 USDC, allowance 100 USDC to the controller and a wallet daily limit of 50 USDC; card_A of tenant A on that
// wallet, daily limit 200 USDC, ACTIVE.
func newEnv(t *testing.T, o options) *env {
	t.Helper()
	owner := testdb.Open(t)
	testdb.Clean(t)
	e := &env{owner: owner, cardAuth: testdb.OpenCardAuth(t), clock: &fakeClock{}, logs: &syncBuffer{}}
	e.clock.Set(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))

	e.chain = o.chain
	if e.chain == nil {
		e.chain = testchain.Start(t)
	}
	e.wallet = e.chain.NewWallet(t)
	e.chain.Mint(t, e.wallet, usdc(t, "100"))
	e.chain.Approve(t, e.wallet, usdc(t, "100"))
	e.chain.SetDailyLimit(t, e.wallet, usdc(t, "50"))

	reg := registry.New(owner)
	tenant, err := reg.CreateTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	e.tenantA = tenant.ID
	if e.pairA, err = reg.IssueProcessorCredential(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	e.cardA = e.addCard(t, e.tenantA, "card_A", e.wallet, usdc(t, "200"))

	rpcURL := o.rpcURL
	if rpcURL == "" {
		rpcURL = e.chain.RPCURL
	}
	client, err := chain.Dial(ctx, "CARD_AUTH_RPC_URL", vault.NewSecret(rpcURL), "CARD_AUTH_RPC_FALLBACK_URL", vault.NewSecret(o.fallbackURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	logger := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.reader, err = chain.NewReader(client, chain.ReaderConfig{
		ChainID: testchain.ChainID, Controller: e.chain.Controller, Token: e.chain.Token,
		ReadTimeout: or(o.readTimeout, 500*time.Millisecond), FallbackAfter: or(o.fallbackAfter, 3), ProbeInterval: or(o.probe, 2*time.Second),
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	probeCtx, stopProbe := context.WithCancel(ctx)
	t.Cleanup(stopProbe)
	go e.reader.Probe(probeCtx)

	var rates decision.Rates
	if !o.noCRS {
		e.crs = crstest.Start(t)
		addr := o.crsAddr
		if addr == "" {
			addr = e.crs.Addr
		}
		rc, err := crs.Dial(addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rc.Close() })
		rates = rc
	}

	var db decision.DB = e.cardAuth
	if o.db != nil {
		db = o.db(e.cardAuth)
	}
	e.debit = o.debit
	if e.debit == nil {
		e.debit = &fakeDebit{}
	}
	bufferBPS := 100
	if o.bufferBPS != nil {
		bufferBPS = *o.bufferBPS
	}
	e.engine = &spyEngine{inner: decision.New(db, rates, e.reader, e.debit, decision.Config{
		DecisionDeadline: or(o.deadline, 2500*time.Millisecond), QuoteBufferBPS: bufferBPS,
		TokenDecimals: testchain.TokenDecimals, Token: "USDC", ChainID: testchain.ChainID,
	}, logger, e.clock.Now)}

	var readsDB decision.DB = e.cardAuth
	if o.readsDB != nil {
		readsDB = o.readsDB(e.cardAuth)
	}
	e.metrics = prometheus.NewRegistry()
	e.srv = httptest.NewServer(processorapi.NewHandler(processorapi.Deps{
		DB: readsDB, Engine: e.engine, Reads: registry.NewCards(readsDB, e.clock.Now),
		Metrics: processorapi.NewMetrics(e.metrics), Logger: logger,
	}))
	t.Cleanup(e.srv.Close)

	doc, err := openapi.Load()
	if err != nil {
		t.Fatal(err)
	}
	if e.router, err = legacy.NewRouter(doc); err != nil {
		t.Fatal(err)
	}
	return e
}

func or[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// addCard registers a card of the tenant on a wallet connection of the local chain.
func (e *env) addCard(t *testing.T, tenantID uuid.UUID, cardRef string, wallet common.Address, limit *big.Int) uuid.UUID {
	t.Helper()
	var connID, cardID uuid.UUID
	if err := e.owner.QueryRow(ctx, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account)
		VALUES ($1, 'owner-a', (SELECT id FROM sources WHERE code = 'anvil'), $2)
		ON CONFLICT (tenant_id, source_id, external_account) DO UPDATE SET label = NULL RETURNING id`,
		tenantID, wallet.Hex()).Scan(&connID); err != nil {
		t.Fatal(err)
	}
	if err := e.owner.QueryRow(ctx, `INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, status, daily_limit)
		VALUES ($1, $2, 'owner-a', $3, 'ACTIVE', $4::text::numeric) RETURNING id`,
		tenantID, cardRef, connID, limit.String()).Scan(&cardID); err != nil {
		t.Fatal(err)
	}
	return cardID
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.owner.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// response is a decoded response body.
type response struct {
	status int
	body   map[string]any
	raw    []byte
}

func (r response) str(path ...string) string {
	var v any = r.body
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[p]
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		b, _ := json.Marshal(x)
		return string(b)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// do sends a request and validates the response against the OpenAPI contract (S2-T203).
func (e *env) do(t *testing.T, method, path string, body []byte, setAuth func(*http.Request)) response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if setAuth != nil {
		setAuth(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.validate(req, resp, raw); err != nil {
		t.Errorf("S2-T203: %s %s → %d does not match the OpenAPI contract: %v\n%s", method, path, resp.StatusCode, err, raw)
	}
	out := response{status: resp.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// validate checks status, headers and body of a response of the processor API against api/openapi.
func (e *env) validate(req *http.Request, resp *http.Response, body []byte) error {
	// The router matches the decoded path: an auth_id with an encoded slash would span two segments. It is
	// routed on the escaped path, which keeps the parameter in one segment.
	routed := req.Clone(req.Context())
	routed.URL.Path, routed.URL.RawPath = req.URL.EscapedPath(), ""
	route, params, err := e.router.FindRoute(routed)
	if err != nil {
		return err
	}
	in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
	return openapi3filter.ValidateResponse(ctx, &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: resp.StatusCode, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	})
}

func basic(pair registry.IssuedPair) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(pair.Username, pair.Password) }
}

// authorize sends POST /v1/authorizations as tenant A.
func (e *env) authorize(t *testing.T, body any) response {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/authorizations", mustJSON(t, body), basic(e.pairA))
}

// get sends GET /v1/authorizations/{auth_id} as tenant A; authID is put into the path as given.
func (e *env) get(t *testing.T, authID string) response {
	t.Helper()
	return e.do(t, http.MethodGet, "/v1/authorizations/"+authID, nil, basic(e.pairA))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	if b, ok := v.([]byte); ok {
		return b
	}
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// authReq is a request body of POST /v1/authorizations.
func authReq(authID, cardRef, amount, currency string) map[string]any {
	return map[string]any{"auth_id": authID, "card_ref": cardRef, "amount": amount, "currency": currency}
}

// declinedWith checks a 200 DECLINED answer with the reason and the status.
func declinedWith(t *testing.T, r response, status, reason string) {
	t.Helper()
	if r.status != http.StatusOK || r.str("decision") != decision.Declined || r.str("status") != status ||
		r.str("decline_reason") != reason {
		t.Errorf("answer %d %s, want 200 DECLINED, status %s, reason %s", r.status, r.raw, status, reason)
	}
}

// row is an authorization as stored, read with the owner role.
type row struct {
	Status, DeclineReason, Rate, Token, TokenAmount, Amount, Currency string
	BufferBps                                                         *int32
	CardID                                                            *uuid.UUID
	Wallet                                                            []byte
	ChainID                                                           *int64
	ChainAuthID, RequestHash                                          []byte
	ReceivedAt                                                        time.Time
	DeadlineAt, DecidedAt                                             *time.Time
	Events                                                            []string // from>to:reason
}

func (e *env) row(t *testing.T, tenantID uuid.UUID, authID string) row {
	t.Helper()
	var r row
	var id uuid.UUID
	err := e.owner.QueryRow(ctx, `SELECT id, status, COALESCE(decline_reason, ''), COALESCE(rate::text, ''),
		COALESCE(token, ''), COALESCE(token_amount::text, ''), COALESCE(fiat_amount::text, ''), COALESCE(fiat_currency, ''),
		buffer_bps, card_id, wallet_address, chain_id, chain_auth_id, request_hash, received_at, deadline_at, decided_at
		FROM authorizations WHERE tenant_id = $1 AND auth_id = $2`, tenantID, authID).Scan(&id, &r.Status, &r.DeclineReason,
		&r.Rate, &r.Token, &r.TokenAmount, &r.Amount, &r.Currency, &r.BufferBps, &r.CardID, &r.Wallet, &r.ChainID,
		&r.ChainAuthID, &r.RequestHash, &r.ReceivedAt, &r.DeadlineAt, &r.DecidedAt)
	if err != nil {
		t.Fatalf("read authorization %q: %v", authID, err)
	}
	rows, err := e.owner.Query(ctx, `SELECT COALESCE(from_status, '') || '>' || to_status || ':' || COALESCE(reason, '')
		FROM authorization_events WHERE authorization_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Events, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	return r
}

// noTransaction checks that the operator sent nothing: no code of this stage sends a transaction.
func (e *env) noTransaction(t *testing.T) {
	t.Helper()
	if n := e.chain.TxCount(t, e.chain.Operator); n != 0 {
		t.Errorf("the operator sent %d transactions", n)
	}
}

// decisions returns auth_decisions_total{decision,reason}.
func (e *env) decisions(t *testing.T, dec, reason string) float64 {
	t.Helper()
	families, err := e.metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "auth_decisions_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["decision"] == dec && labels["reason"] == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// observations returns the number of auth_decision_seconds observations.
func (e *env) observations(t *testing.T) uint64 {
	t.Helper()
	families, err := e.metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "auth_decision_seconds" {
			return f.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	return 0
}

// fakeDebit is the Debit step of the tests: it records what it gets. By default it fails, so the engine
// declines as INTERNAL_ERROR, as the binary of this stage does.
type fakeDebit struct {
	mu    sync.Mutex
	got   []decision.Checked
	block chan struct{} // when set, Debit waits for it to close
	reply func(decision.Checked) (decision.Decision, error)
}

var errFakeDebit = errors.New("fake debit: no transaction in st5a")

func (d *fakeDebit) Debit(ctx context.Context, a decision.Checked) (decision.Decision, error) {
	d.mu.Lock()
	d.got = append(d.got, a)
	block, reply := d.block, d.reply
	d.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	if reply != nil {
		return reply(a)
	}
	return decision.Decision{}, errFakeDebit
}

func (d *fakeDebit) calls() []decision.Checked {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]decision.Checked(nil), d.got...)
}

// spyEngine counts the requests that reach the engine.
type spyEngine struct {
	inner *decision.Engine
	n     atomic.Int64
}

func (s *spyEngine) Authorize(ctx context.Context, req decision.Request, start time.Time) (decision.Result, error) {
	s.n.Add(1)
	return s.inner.Authorize(ctx, req, start)
}

// fakeClock is the clock of received_at and of the UTC day of step 8.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

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

// failingDB fails every statement whose SQL contains match with a database error.
type failingDB struct {
	*pgxpool.Pool
	match string
}

func (d failingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failingTx{Tx: tx, match: d.match}, nil
}

func (d failingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, d.match) {
		return errRow{}
	}
	return d.Pool.QueryRow(ctx, sql, args...)
}

type failingTx struct {
	pgx.Tx
	match string
}

func (tx failingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, tx.match) {
		return errRow{}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

type errRow struct{}

func (errRow) Scan(...any) error {
	return &pgconn.PgError{Code: "08006", Message: "injected failure"}
}

// rpcProxy stands between card-auth and Anvil: it records every request and can fail or delay them.
type rpcProxy struct {
	srv    *httptest.Server
	target string

	mu       sync.Mutex
	mode     string // "", "error", "delay"
	delay    time.Duration
	requests []proxied
}

type proxied struct {
	body     []byte
	duration time.Duration
}

func newRPCProxy(t *testing.T, target string) *rpcProxy {
	t.Helper()
	p := &rpcProxy{target: target}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *rpcProxy) set(mode string, delay time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode, p.delay = mode, delay
}

func (p *rpcProxy) take() []proxied {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.requests
	p.requests = nil
	return out
}

func (p *rpcProxy) serve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	mode, delay := p.mode, p.delay
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.requests = append(p.requests, proxied{body: body, duration: time.Since(start)})
		p.mu.Unlock()
	}()

	switch mode {
	case "error":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonRPCErrors(body))
		return
	case "delay":
		time.Sleep(delay)
	}
	resp, err := http.Post(p.target, "application/json", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// jsonRPCErrors answers every call of a request, single or batch, with a JSON-RPC error.
func jsonRPCErrors(body []byte) []byte {
	type msg struct {
		ID json.RawMessage `json:"id"`
	}
	answer := func(id json.RawMessage) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": "injected failure"}}
	}
	var batch []msg
	if json.Unmarshal(body, &batch) == nil {
		out := make([]map[string]any, 0, len(batch))
		for _, m := range batch {
			out = append(out, answer(m.ID))
		}
		b, _ := json.Marshal(out)
		return b
	}
	var single msg
	_ = json.Unmarshal(body, &single)
	b, _ := json.Marshal(answer(single.ID))
	return b
}

// closedURL is an HTTP URL on a loopback port that nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr
}
