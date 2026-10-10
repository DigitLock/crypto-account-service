// Phase 3 of docs/test-plan-x1.md through CreateConnection of the server of cmd/server in process: the Binance
// connector against the fake Binance server of internal/httpfixture, on the fixtures of testdata/fixtures/binance/.
// The names carry the X1 row and differ from the C1 rows of the other files.
package api_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/binance"
	"github.com/DigitLock/crypto-account-service/internal/connector/connectortest"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// binanceFixtures is testdata/fixtures/binance/ of the repository, seen from this package.
var binanceFixtures = filepath.Join("..", "..", "..", "testdata", "fixtures", "binance")

// fixtureUID is the fictitious uid of the fixtures.
const fixtureUID = "100000001"

const pathRestrictions = "/sapi/v1/account/apiRestrictions"

// permissionsBeyondReading are the permissions of apiRestrictions beyond reading as of 2026-10-09 (UC-201 step 3).
var permissionsBeyondReading = []string{
	"enableWithdrawals", "enableInternalTransfer", "permitsUniversalTransfer", "enableSpotAndMarginTrading",
	"enableMargin", "enableFutures", "enableVanillaOptions", "enablePortfolioMarginTrading", "enableFixApiTrade",
}

func binanceFixture(t *testing.T, name string) httpfixture.File {
	t.Helper()
	f, err := httpfixture.Read(filepath.Join(binanceFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// withRestriction returns key_read_only.json with one field of the apiRestrictions answer set. Without the account
// call when the key check stops at apiRestrictions.
func withRestriction(t *testing.T, field string, value any, accountRead bool) httpfixture.File {
	t.Helper()
	f := binanceFixture(t, "key_read_only.json")
	var calls []httpfixture.Call
	for _, c := range f.Calls {
		switch c.Path {
		case pathRestrictions:
			var body map[string]any
			if err := json.Unmarshal(c.Body, &body); err != nil {
				t.Fatal(err)
			}
			body[field] = value
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			c.Body = b
		case "/api/v3/account":
			if !accountRead {
				continue
			}
		}
		calls = append(calls, c)
	}
	f.Description = "key_read_only.json with " + field + " set"
	f.Calls = calls
	return f
}

// restrictionsAnswer returns key_rejected.json with the answer of apiRestrictions replaced.
func restrictionsAnswer(t *testing.T, status int, body string) httpfixture.File {
	t.Helper()
	f := binanceFixture(t, "key_rejected.json")
	for i := range f.Calls {
		if f.Calls[i].Path == pathRestrictions {
			f.Calls[i].HTTPStatus, f.Calls[i].Body = status, nil
			if body != "" {
				f.Calls[i].Body = []byte(body)
			}
		}
	}
	return f
}

// serveBinance serves f and points base_url of the seeded source binance at it; the config of the source is
// restored after the test (internal/testdb Clean keeps the seeded sources).
func (e *env) serveBinance(t *testing.T, f httpfixture.File) *httpfixture.Server {
	t.Helper()
	srv := httpfixture.Serve(t, f, nil)
	var before []byte
	if err := e.owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'binance'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := e.owner.Exec(ctx, `UPDATE sources SET config = $1 WHERE code = 'binance'`, before); err != nil {
			t.Errorf("restore the config of binance: %v", err)
		}
	})
	if _, err := e.owner.Exec(ctx, `UPDATE sources SET config = jsonb_set(config, '{base_url}', to_jsonb($1::text))
		WHERE code = 'binance'`, srv.URL()); err != nil {
		t.Fatal(err)
	}
	return srv
}

func binanceRequest(owner string, k exchangeKey) *casv1.CreateConnectionRequest {
	r := exchangeRequest(owner, k)
	r.Source = binance.Code
	return r
}

// createOnBinance serves f, creates a connection with a new key in a new tenant and checks that every call of f
// was served.
func (e *env) createOnBinance(t *testing.T, tenant string, f httpfixture.File) (*casv1.CreateConnectionResponse, caller, exchangeKey, error) {
	t.Helper()
	srv := e.serveBinance(t, f)
	who := e.caller(t, tenant)
	k := newKey(t)
	resp, err := client(e.realServer(t)).CreateConnection(who.ctx, binanceRequest("owner-1", k))
	srv.AssertAllServed()
	return resp, who, k, err
}

// assertNoSecretLogged: the key and the secret of the test appear in no log line of the server.
func assertNoSecretLogged(t *testing.T, e *env, k exchangeKey) {
	t.Helper()
	if out := e.log.String(); strings.Contains(out, k.apiKey) || strings.Contains(out, k.apiSecret) {
		t.Error("the log of the server contains the key or the secret")
	}
}

// X1-T301 — Req: UC-201, FR-202; Core UC-101; §3.2 Performance. A read-only key: ACTIVE, EXCHANGE, ["READ"],
// fingerprint; external_account the fictitious uid; the cursor balances due at once (st5, X1 D-33); audit
// CONNECTION_CREATED; reservations 1 in the budget of apiRestrictions and 20 in api besides the time call.
func TestT301_ReadOnlyKey(t *testing.T) {
	e := setup(t)
	resp, who, k, err := e.createOnBinance(t, "tenant-a", binanceFixture(t, "key_read_only.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := resp.GetConnection()
	if c.GetSource() != "binance" || c.GetKind() != casv1.ConnectionKind_CONNECTION_KIND_EXCHANGE ||
		c.GetStatus() != casv1.ConnectionStatus_CONNECTION_STATUS_ACTIVE ||
		c.GetKeyFingerprint() != "…"+k.apiKey[len(k.apiKey)-4:] || !slices.Equal(c.GetPermissions(), []string{"READ"}) {
		t.Errorf("response = %v", c)
	}
	id := uuid.MustParse(c.GetConnectionId())
	var account string
	var enc []byte
	if err := e.owner.QueryRow(ctx, `SELECT external_account, credentials_enc FROM connections WHERE id = $1`, id).
		Scan(&account, &enc); err != nil {
		t.Fatal(err)
	}
	if account != fixtureUID || len(enc) == 0 {
		t.Errorf("external_account %s, ciphertext %d bytes; want %s and a ciphertext", account, len(enc), fixtureUID)
	}
	var cursor string
	if err := e.owner.QueryRow(ctx, `SELECT string_agg(stream || ' ' || mode || ' ' || cursor::text || ' ' || (next_run_at = $2)::text, ',')
		FROM sync_cursors WHERE connection_id = $1`, id, testNow).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != "balances INCREMENTAL {} true" {
		t.Errorf("cursors = %q, want balances INCREMENTAL {} due at once", cursor)
	}
	assertAudit(t, e, id, who.credentialID, map[string]any{
		"source": "binance", "owner_ref": "owner-1", "permissions": []any{"READ"}, "ip_restricted": false,
	})
	assertNoSecretLogged(t, e, k)

	t.Run("reservations", func(t *testing.T) {
		srv := httpfixture.Serve(t, binanceFixture(t, "key_read_only.json"), nil)
		lim := connectortest.NewLimiter()
		src := connector.Source{Code: binance.Code, Kind: connector.KindExchange, Enabled: true,
			Config: []byte(`{"base_url": "` + srv.URL() + `"}`)}
		info, err := binance.New(nil, nil).CheckAccount(ctx, src, connector.Credentials{ExchangeKey: &connector.ExchangeKey{
			APIKey: vault.NewSecret(k.apiKey), APISecret: vault.NewSecret(k.apiSecret)}}, lim)
		if err != nil || info.Identity != fixtureUID || info.IPRestricted == nil || *info.IPRestricted {
			t.Fatalf("CheckAccount = %+v, %v", info, err)
		}
		srv.AssertAllServed()
		want := map[string]int{binance.BudgetAPI: 1 + 20, binance.SAPIBudget(pathRestrictions): 1}
		if got := lim.Reserved(); len(got) != 2 || got[binance.BudgetAPI] != want[binance.BudgetAPI] ||
			got[binance.SAPIBudget(pathRestrictions)] != 1 {
			t.Errorf("reserved = %v, want %v: 1 for the time call, 20 for the account, 1 for apiRestrictions", got, want)
		}
	})
}

// X1-T302 — Req: FR-201, EC-201; Core EC-101, FR-102; X1 P-5. Each of the nine permissions beyond reading:
// FAILED_PRECONDITION / KEY_NOT_READ_ONLY naming the Binance field; nothing stored.
func TestT302_EachPermissionBeyondReading(t *testing.T) {
	e := setup(t)
	if _, _, _, err := e.createOnBinance(t, "tenant-file", binanceFixture(t, "key_not_read_only.json")); !strings.Contains(
		status.Convert(err).Message(), "enableWithdrawals") {
		t.Errorf("key_not_read_only.json: %v", err)
	}
	for _, p := range permissionsBeyondReading {
		_, _, k, err := e.createOnBinance(t, "tenant-"+strings.ToLower(p), withRestriction(t, p, true, false))
		assertReason(t, p, err, codes.FailedPrecondition, "cas/KEY_NOT_READ_ONLY")
		if msg := status.Convert(err).Message(); msg != "the key is not read-only: "+p {
			t.Errorf("%s: message %q", p, msg)
		}
		assertNoSecretLogged(t, e, k)
	}
	assertNothingStored(t, e)
}

// X1-T303 — Req: EC-214, FR-201; X1 D-1. An unknown permission enableNewThing: true is rejected and named; false is
// accepted.
func TestT303_UnknownPermission(t *testing.T) {
	e := setup(t)
	_, _, _, err := e.createOnBinance(t, "tenant-true", withRestriction(t, "enableNewThing", true, false))
	assertReason(t, "enableNewThing true", err, codes.FailedPrecondition, "cas/KEY_NOT_READ_ONLY")
	if msg := status.Convert(err).Message(); msg != "the key is not read-only: enableNewThing" {
		t.Errorf("message %q", msg)
	}
	assertNothingStored(t, e)

	resp, _, _, err := e.createOnBinance(t, "tenant-false", withRestriction(t, "enableNewThing", false, true))
	if err != nil || !slices.Equal(resp.GetConnection().GetPermissions(), []string{"READ"}) {
		t.Errorf("enableNewThing false: %v, %v; want accepted with READ", resp, err)
	}
}

// X1-T304 — Req: EC-214; X1 D-1. Fields that are not permissions, and enableFixReadOnly: accepted with ["READ"].
func TestT304_FieldsThatAreNotPermissions(t *testing.T) {
	e := setup(t)
	for i, c := range []struct {
		field string
		value any
	}{
		{"ipRestrict", true},
		{"createTime", 1700000000000},
		{"enableTradeNote", "a text, not a boolean"},
		{"permitsLevel", 3},
		{"enableFixReadOnly", true},
	} {
		resp, _, _, err := e.createOnBinance(t, "tenant-"+string(rune('a'+i)), withRestriction(t, c.field, c.value, true))
		if err != nil || !slices.Equal(resp.GetConnection().GetPermissions(), []string{"READ"}) {
			t.Errorf("%s = %v: %v, %v; want accepted with READ", c.field, c.value, resp, err)
		}
	}
}

// X1-T305 — Req: UC-201 step 2. enableReading false: KEY_INVALID; nothing stored.
func TestT305_ReadingNotEnabled(t *testing.T) {
	e := setup(t)
	_, _, _, err := e.createOnBinance(t, "tenant-a", binanceFixture(t, "key_reading_disabled.json"))
	assertReason(t, "enableReading false", err, codes.FailedPrecondition, "cas/KEY_INVALID")
	assertNothingStored(t, e)
}

// X1-T306 — Req: EC-202; X1 D-3, X1 D-32. apiRestrictions answers -2015, then 401, -2014, -1022: KEY_INVALID with
// the hint that the key may be restricted to another IP address; nothing stored.
func TestT306_KeyRejectedByBinance(t *testing.T) {
	e := setup(t)
	const hint = "the source rejects the key: it may be mistyped, revoked or restricted to another IP address"
	for i, f := range []httpfixture.File{
		binanceFixture(t, "key_rejected.json"),
		restrictionsAnswer(t, 401, ""),
		restrictionsAnswer(t, 400, `{"code":-2014,"msg":"API-key format invalid."}`),
		restrictionsAnswer(t, 400, `{"code":-1022,"msg":"Signature for this request is not valid."}`),
	} {
		_, _, k, err := e.createOnBinance(t, "tenant-"+string(rune('a'+i)), f)
		assertReason(t, f.Description, err, codes.FailedPrecondition, "cas/KEY_INVALID")
		if msg := status.Convert(err).Message(); msg != hint {
			t.Errorf("%d: message %q, want %q", i, msg, hint)
		}
		assertNoSecretLogged(t, e, k)
	}
	assertNothingStored(t, e)
}

// X1-T307 — Req: UC-201 step 4. apiRestrictions read-only, account answers -2015: KEY_INVALID; nothing stored.
func TestT307_AccountCallRejected(t *testing.T) {
	e := setup(t)
	_, _, _, err := e.createOnBinance(t, "tenant-a", binanceFixture(t, "key_account_rejected.json"))
	assertReason(t, "account rejected", err, codes.FailedPrecondition, "cas/KEY_INVALID")
	assertNothingStored(t, e)
}

// X1-T308 — Req: EC-213; X1 D-2. ipRestrict false, then true: both accepted; the details of CONNECTION_CREATED hold
// ip_restricted; the response is the same.
func TestT308_IPRestrictionNoted(t *testing.T) {
	e := setup(t)
	var responses []*casv1.Connection
	for i, c := range []struct {
		file   string
		wanted bool
	}{{"key_read_only.json", false}, {"key_ip_restricted.json", true}} {
		resp, who, _, err := e.createOnBinance(t, "tenant-"+string(rune('a'+i)), binanceFixture(t, c.file))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		conn := resp.GetConnection()
		assertAudit(t, e, uuid.MustParse(conn.GetConnectionId()), who.credentialID, map[string]any{
			"source": "binance", "owner_ref": "owner-1", "permissions": []any{"READ"}, "ip_restricted": c.wanted,
		})
		responses = append(responses, conn)
	}
	a, b := responses[0], responses[1]
	if a.GetStatus() != b.GetStatus() || a.GetKind() != b.GetKind() || !slices.Equal(a.GetPermissions(), b.GetPermissions()) {
		t.Errorf("responses differ beyond the IDs and the fingerprint: %v / %v", a, b)
	}
}

// X1-T311 — Req: Core EC-104. Binance unreachable (500), then 429 at creation: UNAVAILABLE; nothing stored.
func TestT311_BinanceUnavailableAtCreation(t *testing.T) {
	e := setup(t)
	for i, f := range []httpfixture.File{
		restrictionsAnswer(t, 500, ""),
		restrictionsAnswer(t, 429, ""),
	} {
		_, _, _, err := e.createOnBinance(t, "tenant-"+string(rune('a'+i)), f)
		assertCode(t, "answer "+string(rune('0'+i)), err, codes.Unavailable)
	}
	assertNothingStored(t, e)
}

// X1-T312 — Req: Core EC-102, FR-104. Another key of the same uid in the same tenant: ALREADY_EXISTS.
func TestT312_SameAccountAgain(t *testing.T) {
	e := setup(t)
	read := binanceFixture(t, "key_read_only.json")
	// The second key check finds the time fresh: no time call.
	f := httpfixture.File{Description: "two key checks of one uid", Calls: append(slices.Clone(read.Calls), read.Calls[1:]...)}
	srv := e.serveBinance(t, f)
	who := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	if _, err := c.CreateConnection(who.ctx, binanceRequest("owner-1", newKey(t))); err != nil {
		t.Fatal(err)
	}
	_, err := c.CreateConnection(who.ctx, binanceRequest("owner-2", newKey(t)))
	assertCode(t, "another key of the same uid", err, codes.AlreadyExists)
	srv.AssertAllServed()
	if n := count(t, e, `SELECT count(*) FROM connections`); n != 1 {
		t.Errorf("%d connections, want 1", n)
	}
}
