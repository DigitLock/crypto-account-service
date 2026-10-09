// Phase 5 of docs/test-plan-c1.md (st6a part) and T410: ListSources and CreateConnection through the
// server of cmd/server in process, with the real interceptor, the pool of cas_server, the scripted fake
// connector and a fake clock.
package api_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/connector/fake"
	"github.com/DigitLock/crypto-account-service/internal/grpc/api"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
)

// caller is an authenticated tenant: the context of its calls and the ID of its credential.
type caller struct {
	ctx          context.Context
	tenantID     uuid.UUID
	credentialID uuid.UUID
	token        string
}

func (e *env) caller(t *testing.T, tenant string) caller {
	t.Helper()
	tn := e.tenant(t, tenant)
	tok := e.issue(t, tenant)
	var credentialID uuid.UUID
	if err := e.owner.QueryRow(ctx, `SELECT id FROM api_credentials WHERE key_id = $1`, tok.KeyID).
		Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	return caller{ctx: bearer(tok.Value), tenantID: tn.ID, credentialID: credentialID, token: tok.Value}
}

func client(conn *grpc.ClientConn) casv1.ConnectionServiceClient {
	return casv1.NewConnectionServiceClient(conn)
}

// randomHex returns n lower-case hexadecimal characters generated at run time.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	return hex.EncodeToString(randomBytes(t, (n+1)/2))[:n]
}

// exchangeKey is a key and a secret generated at run time.
type exchangeKey struct{ apiKey, apiSecret string }

func newKey(t *testing.T) exchangeKey {
	t.Helper()
	return exchangeKey{apiKey: randomHex(t, 32), apiSecret: randomHex(t, 64)}
}

func exchangeRequest(owner string, k exchangeKey) *casv1.CreateConnectionRequest {
	return &casv1.CreateConnectionRequest{
		OwnerRef: owner, Source: fake.Code, Label: "Main account",
		Credential: &casv1.CreateConnectionRequest_ExchangeKey{
			ExchangeKey: &casv1.ExchangeKey{ApiKey: k.apiKey, ApiSecret: k.apiSecret},
		},
	}
}

func walletRequest(owner, source, address string) *casv1.CreateConnectionRequest {
	return &casv1.CreateConnectionRequest{
		OwnerRef: owner, Source: source,
		Credential: &casv1.CreateConnectionRequest_Wallet{Wallet: &casv1.Wallet{Address: address}},
	}
}

// newAddress returns a wallet address generated at run time, in lower case.
func newAddress(t *testing.T) string {
	t.Helper()
	return "0x" + randomHex(t, 40)
}

func count(t *testing.T, e *env, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertNothingStored: no connection, no cursor, no connection audit row.
func assertNothingStored(t *testing.T, e *env) {
	t.Helper()
	for table, n := range map[string]int{
		"connections":  count(t, e, `SELECT count(*) FROM connections`),
		"sync_cursors": count(t, e, `SELECT count(*) FROM sync_cursors`),
		"audit_log":    count(t, e, `SELECT count(*) FROM audit_log WHERE action = 'CONNECTION_CREATED'`),
	} {
		if n != 0 {
			t.Errorf("%s holds %d rows, want none", table, n)
		}
	}
}

// reason returns the ErrorInfo of a status error as "domain/reason".
func reason(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetDomain() + "/" + info.GetReason()
		}
	}
	return ""
}

func assertReason(t *testing.T, what string, err error, code codes.Code, want string) {
	t.Helper()
	assertCode(t, what, err, code)
	if got := reason(err); got != want {
		t.Errorf("%s: error detail %q, want %q", what, got, want)
	}
}

// C1-T410 — Req: FR-104, BR-13
func TestT410_SameAccountInTwoTenants(t *testing.T) {
	e := setup(t)
	a, b := e.caller(t, "tenant-a"), e.caller(t, "tenant-b")
	c := client(e.realServer(t))
	k := newKey(t)

	for _, who := range []caller{a, b} {
		if _, err := c.CreateConnection(who.ctx, exchangeRequest("owner-1", k)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if n := count(t, e, `SELECT count(*) FROM connections WHERE external_account = $1`, fake.AccountOf(k.apiKey)); n != 2 {
		t.Errorf("connections of the account = %d, want 2", n)
	}
}

// C1-T501 — Req: §2.1.1
func TestT501_ListSources(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	for _, stmt := range []string{
		`INSERT INTO sources (code, kind, enabled, config) VALUES ('disabled-x', 'EXCHANGE', false, '{}')`,
		`INSERT INTO sources (code, kind, enabled, config) VALUES ('no-connector', 'EXCHANGE', true, '{}')`,
		`INSERT INTO sources (code, kind, enabled, config) VALUES ('evm-outside', 'EVM', true, '{"chain_id": 11155111}')`,
		`INSERT INTO sources (code, kind, enabled, config) VALUES ('evm-string', 'EVM', true, '{"chain_id": "31337"}')`,
		`INSERT INTO sources (code, kind, enabled, config) VALUES ('evm-none', 'EVM', true, '{}')`,
		`INSERT INTO sources (code, kind, enabled, config) VALUES ('evm-disabled', 'EVM', false, '{"chain_id": 31337}')`,
	} {
		if _, err := e.owner.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := client(e.realServer(t)).ListSources(a.ctx, &casv1.ListSourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range resp.GetSources() {
		got = append(got, s.GetCode()+":"+s.GetKind().String())
	}
	want := []string{"anvil:SOURCE_KIND_EVM", "base-sepolia:SOURCE_KIND_EVM", "binance:SOURCE_KIND_EXCHANGE", "fake:SOURCE_KIND_EXCHANGE"}
	if !slices.Equal(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
}

// C1-T502 — Req: UC-101, FR-101, FR-117
func TestT502_CreateExchange(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	k := newKey(t)

	resp, err := client(e.realServer(t)).CreateConnection(a.ctx, exchangeRequest("owner-1", k))
	if err != nil {
		t.Fatal(err)
	}
	c := resp.GetConnection()
	id, err := uuid.Parse(c.GetConnectionId())
	if err != nil {
		t.Fatalf("connection_id %q: %v", c.GetConnectionId(), err)
	}
	if c.GetSource() != "fake" || c.GetKind() != casv1.ConnectionKind_CONNECTION_KIND_EXCHANGE ||
		c.GetOwnerRef() != "owner-1" || c.GetLabel() != "Main account" ||
		c.GetStatus() != casv1.ConnectionStatus_CONNECTION_STATUS_ACTIVE ||
		c.GetKeyFingerprint() != "…"+k.apiKey[len(k.apiKey)-4:] ||
		!slices.Equal(c.GetPermissions(), []string{"READ"}) || !c.GetCreatedAt().AsTime().Equal(testNow) ||
		c.GetWalletAddress() != "" {
		t.Errorf("response = %v", c)
	}

	var account, status string
	var kek int16
	var checked, created time.Time
	if err := e.owner.QueryRow(ctx, `SELECT external_account, status, kek_version, permissions_checked_at, created_at
		FROM connections WHERE id = $1`, id).Scan(&account, &status, &kek, &checked, &created); err != nil {
		t.Fatal(err)
	}
	if account != fake.AccountOf(k.apiKey) || status != "ACTIVE" || kek != 1 || !checked.Equal(testNow) ||
		!created.Equal(testNow) {
		t.Errorf("row: account %s, status %s, kek %d, checked %v, created %v", account, status, kek, checked, created)
	}

	rows, err := e.owner.Query(ctx, `SELECT stream || ' ' || mode || ' ' || cursor::text, next_run_at
		FROM sync_cursors WHERE connection_id = $1 ORDER BY stream`, id)
	if err != nil {
		t.Fatal(err)
	}
	type cursor struct {
		desc string
		next time.Time
	}
	cursors, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cursor, error) {
		var c cursor
		return c, r.Scan(&c.desc, &c.next)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cursors) != 2 || cursors[0].desc != "balances INCREMENTAL {}" || cursors[1].desc != "ops BACKFILL {}" ||
		!cursors[0].next.Equal(testNow) || !cursors[1].next.Equal(testNow) {
		t.Errorf("cursors = %v, want balances and ops due at %v", cursors, testNow)
	}

	assertAudit(t, e, id, a.credentialID, map[string]any{
		"source": "fake", "owner_ref": "owner-1", "permissions": []any{"READ"},
	})
}

// assertAudit checks the one CONNECTION_CREATED row of a connection.
func assertAudit(t *testing.T, e *env, id, credentialID uuid.UUID, wantDetails map[string]any) {
	t.Helper()
	var gotCredential uuid.UUID
	var details []byte
	if err := e.owner.QueryRow(ctx, `SELECT credential_id, details FROM audit_log
		WHERE action = 'CONNECTION_CREATED' AND object_id = $1`, id.String()).Scan(&gotCredential, &details); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(details, &got); err != nil {
		t.Fatal(err)
	}
	if gotCredential != credentialID || fmt.Sprint(got) != fmt.Sprint(wantDetails) {
		t.Errorf("audit: credential %v, details %v; want %v, %v", gotCredential, got, credentialID, wantDetails)
	}
}

// C1-T503 — Req: UC-101, FR-101, FR-302, FR-117
func TestT503_CreateWallet(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	address := newAddress(t)

	resp, err := client(e.realServer(t)).CreateConnection(a.ctx, walletRequest("owner-1", "anvil", address))
	if err != nil {
		t.Fatal(err)
	}
	c := resp.GetConnection()
	if c.GetKind() != casv1.ConnectionKind_CONNECTION_KIND_EVM_WALLET || c.GetSource() != "anvil" ||
		c.GetKeyFingerprint() != "" || len(c.GetPermissions()) != 0 || c.GetLabel() != "" ||
		c.GetWalletAddress() != evm.ToEIP55(address[2:]) {
		t.Errorf("response = %v", c)
	}
	id := uuid.MustParse(c.GetConnectionId())

	var account string
	var nulls bool
	if err := e.owner.QueryRow(ctx, `SELECT external_account, credentials_enc IS NULL AND kek_version IS NULL
		AND key_fingerprint IS NULL AND permissions IS NULL AND permissions_checked_at IS NULL AND label IS NULL
		FROM connections WHERE id = $1`, id).Scan(&account, &nulls); err != nil {
		t.Fatal(err)
	}
	if account != evm.ToEIP55(address[2:]) || !nulls {
		t.Errorf("row: account %s (want the EIP-55 form), key columns empty %v", account, nulls)
	}
	assertAudit(t, e, id, a.credentialID, map[string]any{"source": "anvil", "owner_ref": "owner-1"})
}

// C1-T504 — Req: UC-101 step 1, §2.1.1
func TestT504_Validation(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	k := newKey(t)

	mutate := func(fn func(r *casv1.CreateConnectionRequest)) *casv1.CreateConnectionRequest {
		r := exchangeRequest("owner-1", k)
		fn(r)
		return r
	}
	withKey := func(apiKey, apiSecret string) *casv1.CreateConnectionRequest {
		return exchangeRequest("owner-1", exchangeKey{apiKey: apiKey, apiSecret: apiSecret})
	}
	refused := map[string]*casv1.CreateConnectionRequest{
		"no owner_ref":            mutate(func(r *casv1.CreateConnectionRequest) { r.OwnerRef = "" }),
		"owner_ref of 129":        mutate(func(r *casv1.CreateConnectionRequest) { r.OwnerRef = strings.Repeat("ж", 129) }),
		"label of 65":             mutate(func(r *casv1.CreateConnectionRequest) { r.Label = strings.Repeat("ж", 65) }),
		"no source":               mutate(func(r *casv1.CreateConnectionRequest) { r.Source = "" }),
		"no credential":           mutate(func(r *casv1.CreateConnectionRequest) { r.Credential = nil }),
		"wallet for an exchange":  walletRequest("owner-1", fake.Code, newAddress(t)),
		"exchange_key for anvil":  mutate(func(r *casv1.CreateConnectionRequest) { r.Source = "anvil" }),
		"api_key of 15":           withKey(randomHex(t, 15), k.apiSecret),
		"api_key of 257":          withKey(randomHex(t, 257), k.apiSecret),
		"api_secret of 15":        withKey(k.apiKey, randomHex(t, 15)),
		"api_secret of 4097":      withKey(k.apiKey, randomHex(t, 4097)),
		"api_key of 15 multibyte": withKey(strings.Repeat("ж", 15), k.apiSecret),
	}
	for name, req := range refused {
		_, err := c.CreateConnection(a.ctx, req)
		assertCode(t, name, err, codes.InvalidArgument)
		msg := status.Convert(err).Message()
		if key := req.GetExchangeKey(); key != nil &&
			(strings.Contains(msg, key.GetApiKey()) || strings.Contains(msg, key.GetApiSecret())) {
			t.Errorf("%s: the message %q contains the key or the secret", name, msg)
		}
	}
	assertNothingStored(t, e)

	accepted := map[string]*casv1.CreateConnectionRequest{
		"owner_ref of 128 multibyte": mutate(func(r *casv1.CreateConnectionRequest) {
			r.OwnerRef = strings.Repeat("ж", 128)
			r.GetExchangeKey().ApiKey = randomHex(t, 32)
		}),
		"label of 64 multibyte": mutate(func(r *casv1.CreateConnectionRequest) {
			r.Label = strings.Repeat("ж", 64)
			r.GetExchangeKey().ApiKey = randomHex(t, 32)
		}),
		"api_key of 16":      withKey(randomHex(t, 16), k.apiSecret),
		"api_key of 256":     withKey(randomHex(t, 256), k.apiSecret),
		"api_secret of 16":   withKey(randomHex(t, 32), randomHex(t, 16)),
		"api_secret of 4096": withKey(randomHex(t, 32), randomHex(t, 4096)),
	}
	for name, req := range accepted {
		if _, err := c.CreateConnection(a.ctx, req); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// C1-T505 — Req: UC-101 step 2, §2.1.1
func TestT505_SourceUnknownOrDisabled(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	if _, err := e.owner.Exec(ctx, `INSERT INTO sources (code, kind, enabled, config)
		VALUES ('disabled-x', 'EXCHANGE', false, '{}')`); err != nil {
		t.Fatal(err)
	}
	c := client(e.realServer(t))

	req := exchangeRequest("owner-1", newKey(t))
	req.Source = "no-such-source"
	_, err := c.CreateConnection(a.ctx, req)
	assertCode(t, "unknown source", err, codes.NotFound)

	req.Source = "disabled-x"
	_, err = c.CreateConnection(a.ctx, req)
	assertReason(t, "disabled source", err, codes.FailedPrecondition, "cas/SOURCE_DISABLED")
	assertNothingStored(t, e)
}

// C1-T506 — Req: EC-101, FR-102
func TestT506_KeyNotReadOnly(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))

	for name, script := range map[string]func(){
		"permission reported": func() {
			e.fake.SetCheckResult(connector.AccountInfo{Identity: "fake-1", Permissions: []string{"READ", "TRADE"}}, nil)
		},
		"typed error": func() {
			e.fake.SetCheckResult(connector.AccountInfo{}, &connector.KeyNotReadOnlyError{Permissions: []string{"TRADE"}})
		},
	} {
		script()
		_, err := c.CreateConnection(a.ctx, exchangeRequest("owner-1", newKey(t)))
		assertReason(t, name, err, codes.FailedPrecondition, "cas/KEY_NOT_READ_ONLY")
		if !strings.Contains(status.Convert(err).Message(), "TRADE") {
			t.Errorf("%s: message %q does not name the permission", name, status.Convert(err).Message())
		}
	}
	assertNothingStored(t, e)
	if n := count(t, e, `SELECT count(*) FROM connections WHERE credentials_enc IS NOT NULL`); n != 0 {
		t.Errorf("%d ciphertexts stored", n)
	}
}

// C1-T507 — Req: UC-101 step 3
func TestT507_KeyRejected(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	e.fake.SetCheckResult(connector.AccountInfo{}, connector.ErrKeyRejected)

	_, err := client(e.realServer(t)).CreateConnection(a.ctx, exchangeRequest("owner-1", newKey(t)))
	assertReason(t, "rejected key", err, codes.FailedPrecondition, "cas/KEY_INVALID")
	assertNothingStored(t, e)
}

// C1-T508 — Req: EC-104, UC-101 step 3
func TestT508_SourceUnreachable(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))

	pauseBudget := func() {
		// The budget of the source is paused longer than the bounded wait of the key check.
		e.fake.Reset()
		lim, err := e.limiters.For(connector.Source{Code: fake.Code}, e.fake)
		if err != nil {
			t.Fatal(err)
		}
		lim.Pause(fake.DefaultBudget, time.Hour)
	}
	endPause := func(err error) func() {
		return func() {
			e.now = e.now.Add(2 * time.Hour)
			e.fake.SetCheckResult(connector.AccountInfo{}, err)
		}
	}
	script := func(err error) func() {
		return func() { e.fake.SetCheckResult(connector.AccountInfo{}, err) }
	}
	for _, step := range []struct {
		name    string
		prepare func()
		want    codes.Code
	}{
		{"unreachable", script(fmt.Errorf("dial: %w", connector.ErrUnreachable)), codes.Unavailable},
		{"rate limit", script(&connector.RateLimitError{Pause: time.Minute}), codes.Unavailable},
		{"budget paused longer than the wait", pauseBudget, codes.Unavailable},
		{"other failure", endPause(errors.New("fake: unexpected answer")), codes.Internal},
		{"invalid input", script(&connector.InvalidInputError{Message: "the key does not suit the source"}), codes.InvalidArgument},
	} {
		step.prepare()
		_, err := c.CreateConnection(a.ctx, exchangeRequest("owner-1", newKey(t)))
		assertCode(t, step.name, err, step.want)
	}
	assertNothingStored(t, e)
}

// C1-T509 — Req: EC-103, FR-117
func TestT509_PermissionsNotReported(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	e.fake.SetCapabilities(connector.Capabilities{PermissionsReadable: false})

	resp, err := client(e.realServer(t)).CreateConnection(a.ctx, exchangeRequest("owner-1", newKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetConnection().GetPermissions(); !slices.Equal(got, []string{"UNVERIFIED"}) {
		t.Errorf("permissions = %v, want [UNVERIFIED]", got)
	}
	id := uuid.MustParse(resp.GetConnection().GetConnectionId())
	var stored []string
	if err := e.owner.QueryRow(ctx, `SELECT permissions FROM connections WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stored, []string{"UNVERIFIED"}) {
		t.Errorf("stored permissions = %v", stored)
	}
	assertAudit(t, e, id, a.credentialID, map[string]any{
		"source": "fake", "owner_ref": "owner-1", "permissions": []any{"UNVERIFIED"},
	})
}

// sameAccount scripts the fake so that every key belongs to one account.
func sameAccount(e *env) {
	e.fake.SetCheckResult(connector.AccountInfo{Identity: "fake-account-1", Permissions: []string{"READ"}}, nil)
}

// C1-T510 — Req: EC-102, FR-104
func TestT510_SameAccountAgain(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	sameAccount(e)
	c := client(e.realServer(t))

	if _, err := c.CreateConnection(a.ctx, exchangeRequest("owner-1", newKey(t))); err != nil {
		t.Fatal(err)
	}
	_, err := c.CreateConnection(a.ctx, exchangeRequest("owner-2", newKey(t)))
	assertCode(t, "another key of the same account", err, codes.AlreadyExists)
	if n := count(t, e, `SELECT count(*) FROM connections`); n != 1 {
		t.Errorf("connections = %d, want 1", n)
	}
}

// C1-T511 — Req: FR-104
func TestT511_SameAccountConcurrentRequests(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	req := exchangeRequest("owner-1", newKey(t))

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			_, errs[i] = c.CreateConnection(a.ctx, req)
		})
	}
	close(start)
	wg.Wait()

	var ok, exists int
	for _, err := range errs {
		switch status.Code(err) {
		case codes.OK:
			ok++
		case codes.AlreadyExists:
			exists++
		default:
			t.Errorf("unexpected answer: %v", err)
		}
	}
	if ok != 1 || exists != n-1 {
		t.Errorf("%d created, %d ALREADY_EXISTS; want 1 and %d", ok, exists, n-1)
	}
	if got := count(t, e, `SELECT count(*) FROM connections`); got != 1 {
		t.Errorf("connections = %d, want 1", got)
	}
}

// failingAuditDB wraps the database of Connections: inside a transaction the audit insert fails.
// It goes through registry.DB, the interface the production code uses.
type failingAuditDB struct{ registry.DB }

func (d failingAuditDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failingAuditTx{tx}, nil
}

type failingAuditTx struct{ pgx.Tx }

func (tx failingAuditTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO audit_log") {
		return pgconn.CommandTag{}, errors.New("injected failure at the audit insert")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

// C1-T512 — Req: UC-101 step 8
func TestT512_AtomicInsert(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := dial(t, api.NewServer(e.deps(failingAuditDB{e.server})))

	_, err := client(conn).CreateConnection(a.ctx, exchangeRequest("owner-1", newKey(t)))
	assertCode(t, "create with a failing audit insert", err, codes.Internal)
	if msg := status.Convert(err).Message(); msg != "internal error" {
		t.Errorf("message %q, want the fixed one", msg)
	}
	assertNothingStored(t, e)
	if !strings.Contains(e.log.String(), "injected failure at the audit insert") {
		t.Error("the cause of INTERNAL is not logged")
	}
}

// C1-T513 — Req: FR-103, ADR-4. The stored column; internal/vault holds the unit part.
func TestT513_EncryptedAtRest(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	k := newKey(t)

	resp, err := client(e.realServer(t)).CreateConnection(a.ctx, exchangeRequest("owner-1", k))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(resp.GetConnection().GetConnectionId())
	var block []byte
	var kek int16
	if err := e.owner.QueryRow(ctx, `SELECT credentials_enc, kek_version FROM connections WHERE id = $1`, id).
		Scan(&block, &kek); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{k.apiKey, k.apiSecret} {
		if strings.Contains(string(block), s) || strings.Contains(hex.EncodeToString(block), s) {
			t.Error("credentials_enc holds the key or the secret in clear")
		}
	}
	plaintext, err := e.vault.Decrypt([16]byte(id), block, kek)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]string
	if err := json.Unmarshal(plaintext, &stored); err != nil {
		t.Fatal(err)
	}
	if kek != 1 || len(stored) != 2 || stored["api_key"] != k.apiKey || stored["api_secret"] != k.apiSecret {
		t.Errorf("kek_version %d; decrypted object has %d fields and does not match the key", kek, len(stored))
	}
}

// C1-T517 — Req: FR-103, §2.1.2. The responses and errors of CreateConnection, and the responses of
// GetConnection and ListConnections.
func TestT517_FingerprintOnlyInTheAPI(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))

	var texts []string
	var keys []exchangeKey
	call := func(script func()) {
		script()
		k := newKey(t)
		keys = append(keys, k)
		resp, err := c.CreateConnection(a.ctx, exchangeRequest("owner-1", k))
		if err != nil {
			texts = append(texts, err.Error(), fmt.Sprint(status.Convert(err).Details()))
			return
		}
		js, _ := protojson.Marshal(resp)
		texts = append(texts, string(js))
		if got := resp.GetConnection().GetKeyFingerprint(); got != "…"+k.apiKey[len(k.apiKey)-4:] {
			t.Errorf("fingerprint %q", got)
		}
		got, err := c.GetConnection(a.ctx, &casv1.GetConnectionRequest{ConnectionId: resp.GetConnection().GetConnectionId()})
		if err != nil {
			t.Fatal(err)
		}
		js, _ = protojson.Marshal(got)
		texts = append(texts, string(js))
		if got.GetConnection().GetKeyFingerprint() != resp.GetConnection().GetKeyFingerprint() {
			t.Errorf("GetConnection fingerprint %q", got.GetConnection().GetKeyFingerprint())
		}
	}
	call(e.fake.Reset)
	call(func() {
		e.fake.SetCheckResult(connector.AccountInfo{Identity: "x", Permissions: []string{"READ", "WITHDRAW"}}, nil)
	})
	call(func() { e.fake.SetCheckResult(connector.AccountInfo{}, connector.ErrKeyRejected) })
	call(func() { e.fake.SetCheckResult(connector.AccountInfo{}, errors.New("fake: unexpected answer")) })
	// Validation errors.
	short := exchangeKey{apiKey: randomHex(t, 15), apiSecret: randomHex(t, 64)}
	keys = append(keys, short)
	_, err := c.CreateConnection(a.ctx, exchangeRequest("owner-1", short))
	texts = append(texts, err.Error())

	listed, err := c.ListConnections(a.ctx, &casv1.ListConnectionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.GetConnections()) == 0 {
		t.Fatal("ListConnections returned nothing: the test does not exercise it")
	}
	js, _ := protojson.Marshal(listed)
	texts = append(texts, string(js))

	all := strings.Join(texts, "\n")
	for _, k := range keys {
		if strings.Contains(all, k.apiKey) || strings.Contains(all, k.apiSecret) ||
			strings.Contains(all, k.apiKey[:len(k.apiKey)-4]) {
			t.Errorf("a response or an error contains more of a key than its fingerprint:\n%s", all)
		}
	}
}

// C1-T518 — Req: FR-103, handoff §4. Creation, a sync, a key check and their failures.
func TestT518_NothingSecretInTheLogs(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	failing := client(dial(t, api.NewServer(e.deps(failingAuditDB{e.server}))))

	var keys []exchangeKey
	run := func(cl casv1.ConnectionServiceClient, script func()) {
		script()
		k := newKey(t)
		keys = append(keys, k)
		_, _ = cl.CreateConnection(a.ctx, exchangeRequest("owner-1", k))
	}
	run(c, e.fake.Reset)
	run(c, func() { e.fake.SetCheckResult(connector.AccountInfo{}, connector.ErrKeyRejected) })
	run(c, func() { e.fake.SetCheckResult(connector.AccountInfo{}, connector.ErrUnreachable) })
	run(c, func() { e.fake.SetCheckResult(connector.AccountInfo{}, errors.New("fake: unexpected answer")) })
	run(c, func() {
		e.fake.SetCheckResult(connector.AccountInfo{Identity: "y", Permissions: []string{"TRANSFER"}}, nil)
	})
	run(failing, e.fake.Reset)
	run(c, func() { e.fake.SetCheckResult(connector.AccountInfo{}, &connector.RateLimitError{Pause: time.Second}) })

	// A sync and a key check of the connection created first; the failures quote its key and secret.
	created := keys[0]
	e.fake.Reset()
	eng := e.engine()
	pass := func() {
		t.Helper()
		if err := eng.RunPass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pass()
	e.fake.FailNext("ops", fmt.Errorf("fake: refused key %s with secret %s", created.apiKey, created.apiSecret))
	if _, err := e.owner.Exec(ctx, `UPDATE sync_cursors SET next_run_at = $1`, e.now); err != nil {
		t.Fatal(err)
	}
	pass()
	e.now = e.now.Add(24 * time.Hour)
	e.fake.FailNext(fake.CheckStream, fmt.Errorf("fake: check of key %s with secret %s failed", created.apiKey, created.apiSecret))
	pass()
	e.now = e.now.Add(time.Hour)
	pass()

	out := e.log.String()
	for _, logged := range []string{"create connection failed", "sync run failed", "key check failed"} {
		if !strings.Contains(out, logged) {
			t.Fatalf("%q was not logged: the test does not exercise it", logged)
		}
	}
	for _, k := range keys {
		if strings.Contains(out, k.apiKey) || strings.Contains(out, k.apiSecret) {
			t.Error("the log contains an API key or a secret")
		}
	}
	if strings.Contains(out, a.token) {
		t.Error("the log contains the service token")
	}
	// The token and the master key are also checked by the cleanup of setup.
}

// assertWallet sends a wallet request on anvil and expects code.
func assertWallet(t *testing.T, c casv1.ConnectionServiceClient, callCtx context.Context, name, address string, code codes.Code) {
	t.Helper()
	_, err := c.CreateConnection(callCtx, walletRequest("owner-1", "anvil", address))
	assertCode(t, name, err, code)
}

// C1-T527 — Req: UC-301, FR-301
func TestT527_AddressFormat(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	digits := randomHex(t, 40)

	for name, address := range map[string]string{
		"39 characters":       "0x" + digits[:39],
		"41 characters":       "0x" + digits + "a",
		"no prefix":           digits,
		"prefix 0X":           "0X" + digits,
		"non-hexadecimal":     "0x" + digits[:39] + "g",
		"leading whitespace":  " 0x" + digits,
		"trailing whitespace": "0x" + digits + " ",
		"empty":               "",
	} {
		assertWallet(t, c, a.ctx, name, address, codes.InvalidArgument)
	}
	assertNothingStored(t, e)
}

// C1-T528 — Req: EC-301, FR-301
func TestT528_WrongChecksum(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))

	checksummed := evm.ToEIP55(addressWithLetters(t)[2:])
	// Flip the case of one letter: still mixed case, the checksum fails.
	b := []byte(checksummed)
	for i := 2; i < len(b); i++ {
		if b[i] >= 'a' && b[i] <= 'f' {
			b[i] -= 'a' - 'A'
			break
		} else if b[i] >= 'A' && b[i] <= 'F' {
			b[i] += 'a' - 'A'
			break
		}
	}
	assertWallet(t, c, a.ctx, "one letter in the wrong case", string(b), codes.InvalidArgument)
	assertNothingStored(t, e)
}

// addressWithLetters returns a generated address whose EIP-55 form is mixed case.
func addressWithLetters(t *testing.T) string {
	t.Helper()
	for {
		address := newAddress(t)
		cs := evm.ToEIP55(address[2:])
		if strings.ToLower(cs) != cs && strings.ToUpper(cs[2:]) != cs[2:] {
			return address
		}
	}
}

// C1-T529 — Req: FR-301. The test vectors of EIP-55, the only real addresses of the repository.
func TestT529_ValidChecksum(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))

	for _, address := range []string{
		"0x52908400098527886E0F7030069857D2E4169EE7",
		"0x8617E340B3D01FA5F11F306F4090FD50E238070D",
		"0xde709f2102306220921060314715629080e2fb77",
		"0x27b1fdb04752bbc536007a920d24acb045561c26",
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	} {
		assertWallet(t, c, a.ctx, address, address, codes.OK)
	}
}

// C1-T530 — Req: FR-301, FR-302. Stored and returned in EIP-55 form: wallet_address of create, get and list.
func TestT530_SingleCase(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))

	lower := addressWithLetters(t)
	upperDigits := strings.ToUpper(addressWithLetters(t)[2:])
	for _, address := range []string{lower, "0x" + upperDigits} {
		resp, err := c.CreateConnection(a.ctx, walletRequest("owner-1", "anvil", address))
		if err != nil {
			t.Fatalf("%s: %v", address, err)
		}
		var stored string
		if err := e.owner.QueryRow(ctx, `SELECT external_account FROM connections WHERE id = $1`,
			resp.GetConnection().GetConnectionId()).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		want := evm.ToEIP55(address[2:])
		if stored != want || stored == address {
			t.Errorf("%s stored as %s, want its EIP-55 form", address, stored)
		}
		id := resp.GetConnection().GetConnectionId()
		got, err := c.GetConnection(a.ctx, &casv1.GetConnectionRequest{ConnectionId: id})
		if err != nil {
			t.Fatal(err)
		}
		listed, err := c.ListConnections(a.ctx, &casv1.ListConnectionsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		returned := []string{resp.GetConnection().GetWalletAddress(), got.GetConnection().GetWalletAddress()}
		for _, l := range listed.GetConnections() {
			if l.GetConnectionId() == id {
				returned = append(returned, l.GetWalletAddress())
			}
		}
		if len(returned) != 3 || returned[0] != want || returned[1] != want || returned[2] != want {
			t.Errorf("%s returned as %v by create, get and list; want %s", address, returned, want)
		}
	}
}

// C1-T531 — Req: EC-302
func TestT531_ZeroAddress(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	assertWallet(t, client(e.realServer(t)), a.ctx, "zero address", "0x"+strings.Repeat("0", 40), codes.InvalidArgument)
	assertNothingStored(t, e)
}

// C1-T532 — Req: EC-303, FR-302, FR-104
func TestT532_SameAddressInAnotherCase(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	address := addressWithLetters(t)

	assertWallet(t, c, a.ctx, "lower case", address, codes.OK)
	assertWallet(t, c, a.ctx, "EIP-55 form", evm.ToEIP55(address[2:]), codes.AlreadyExists)
	assertWallet(t, c, a.ctx, "upper case", "0x"+strings.ToUpper(address[2:]), codes.AlreadyExists)
}

// C1-T533 — Req: FR-104
func TestT533_SameAddressOnTwoNetworks(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	address := newAddress(t)

	for _, source := range []string{"anvil", "base-sepolia"} {
		if _, err := c.CreateConnection(a.ctx, walletRequest("owner-1", source, address)); err != nil {
			t.Errorf("%s: %v", source, err)
		}
	}
	if n := count(t, e, `SELECT count(*) FROM connections`); n != 2 {
		t.Errorf("connections = %d, want 2", n)
	}
}

// C1-T537 — Req: UC-101 postcondition, SRS — EVM §2.1.1. No cursor, and GetConnection returns no stream.
func TestT537_WalletWithoutStreams(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")

	resp, err := client(e.realServer(t)).CreateConnection(a.ctx, walletRequest("owner-1", "anvil", newAddress(t)))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetConnection().GetConnectionId()
	if n := count(t, e, `SELECT count(*) FROM sync_cursors WHERE connection_id = $1`, id); n != 0 {
		t.Errorf("wallet connection has %d cursors, want none", n)
	}
	got, err := client(e.realServer(t)).GetConnection(a.ctx, &casv1.GetConnectionRequest{ConnectionId: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetStreams()) != 0 {
		t.Errorf("streams = %v, want none", got.GetStreams())
	}
}
