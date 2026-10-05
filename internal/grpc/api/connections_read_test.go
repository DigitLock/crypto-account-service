// Phase 5 of docs/test-plan-c1.md (st6b part) and T408, T409: GetConnection, ListConnections and
// DeleteConnection through the server of cmd/server in process.
package api_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/DigitLock/crypto-account-service/internal/connector/fake"
	"github.com/DigitLock/crypto-account-service/internal/grpc/api"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
)

// createExchange creates an exchange connection on fake and returns its ID and key.
func createExchange(t *testing.T, c casv1.ConnectionServiceClient, who caller, owner string) (string, exchangeKey) {
	t.Helper()
	k := newKey(t)
	resp, err := c.CreateConnection(who.ctx, exchangeRequest(owner, k))
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetConnection().GetConnectionId(), k
}

// createWallet creates a wallet connection on anvil and returns its ID.
func createWallet(t *testing.T, c casv1.ConnectionServiceClient, who caller, owner string) string {
	t.Helper()
	resp, err := c.CreateConnection(who.ctx, walletRequest(owner, "anvil", newAddress(t)))
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetConnection().GetConnectionId()
}

func listIDs(t *testing.T, c casv1.ConnectionServiceClient, who caller, req *casv1.ListConnectionsRequest) []string {
	t.Helper()
	resp, err := c.ListConnections(who.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, conn := range resp.GetConnections() {
		ids = append(ids, conn.GetConnectionId())
	}
	return ids
}

// rowsOf counts the rows of a connection in the tables the delete must empty.
func rowsOf(t *testing.T, e *env, id string) map[string]int {
	t.Helper()
	return map[string]int{
		"connections":       count(t, e, `SELECT count(*) FROM connections WHERE id = $1`, id),
		"sync_cursors":      count(t, e, `SELECT count(*) FROM sync_cursors WHERE connection_id = $1`, id),
		"balance_snapshots": count(t, e, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, id),
		"snapshot_balances": count(t, e, `SELECT count(*) FROM snapshot_balances b JOIN balance_snapshots s
			ON s.id = b.snapshot_id WHERE s.connection_id = $1`, id),
		"ledger_entries": count(t, e, `SELECT count(*) FROM ledger_entries WHERE connection_id = $1`, id),
	}
}

// addHistory inserts a snapshot with a balance and a ledger entry of a connection through the owner pool.
// It returns the snapshot ID.
func addHistory(t *testing.T, e *env, who caller, id string) string {
	t.Helper()
	var snapshotID string
	if err := e.owner.QueryRow(ctx, `INSERT INTO balance_snapshots (connection_id, taken_at) VALUES ($1, $2)
		RETURNING id`, id, testNow).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO snapshot_balances (snapshot_id, account_type, native_asset, asset, free, locked)
			VALUES ($1, 'SPOT', 'BTC', 'BTC', 0.0125, 0)`,
	} {
		if _, err := e.owner.Exec(ctx, stmt, snapshotID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.owner.Exec(ctx, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg,
		group_id, type, direction, asset, native_asset, amount, occurred_at, raw)
		VALUES ($1, $2, 'ops', 'op-1', 'SINGLE', 'ops:op-1', 'DEPOSIT', 'IN', 'BTC', 'BTC', 0.0125, $3, '{}')`,
		who.tenantID, id, testNow); err != nil {
		t.Fatal(err)
	}
	return snapshotID
}

// C1-T408 — Req: FR-105, BR-13, G-6. GetConnection and ListConnections. Missing until st8: GetBalances and
// ListLedgerEntries as tenant B.
func TestT408_IsolationOfReads(t *testing.T) {
	e := setup(t)
	a, b := e.caller(t, "tenant-a"), e.caller(t, "tenant-b")
	c := client(e.realServer(t))
	idA, _ := createExchange(t, c, a, "owner-1")
	idB := createWallet(t, c, b, "owner-1")

	_, err := c.GetConnection(b.ctx, &casv1.GetConnectionRequest{ConnectionId: idA})
	assertCode(t, "B reads A's connection", err, codes.NotFound)
	_, errUnknown := c.GetConnection(b.ctx, &casv1.GetConnectionRequest{ConnectionId: uuid.NewString()})
	if status.Convert(err).Message() != status.Convert(errUnknown).Message() {
		t.Errorf("another tenant's connection %q differs from an unknown ID %q", err, errUnknown)
	}

	for _, req := range []*casv1.ListConnectionsRequest{{}, {OwnerRef: "owner-1"}} {
		if got := listIDs(t, c, b, req); !slices.Equal(got, []string{idB}) {
			t.Errorf("B lists %v with owner_ref %q, want only its own %s", got, req.GetOwnerRef(), idB)
		}
	}
	if got := listIDs(t, c, a, &casv1.ListConnectionsRequest{OwnerRef: "owner-1"}); !slices.Equal(got, []string{idA}) {
		t.Errorf("A lists %v, want only %s", got, idA)
	}
}

// C1-T409 — Req: FR-105, BR-13. DeleteConnection. Missing until st8: TriggerSync as tenant B.
func TestT409_IsolationOfWrites(t *testing.T) {
	e := setup(t)
	a, b := e.caller(t, "tenant-a"), e.caller(t, "tenant-b")
	c := client(e.realServer(t))
	idA, _ := createExchange(t, c, a, "owner-1")
	addHistory(t, e, a, idA)

	snapshot := func() string {
		var s string
		if err := e.owner.QueryRow(ctx, `SELECT
			(SELECT string_agg(x::text, ';') FROM connections x WHERE x.tenant_id = $1) || '|' ||
			(SELECT string_agg(x::text, ';' ORDER BY x.stream) FROM sync_cursors x WHERE x.connection_id = $2) || '|' ||
			(SELECT count(*) FROM ledger_entries WHERE connection_id = $2) || '|' ||
			(SELECT string_agg(x::text, ';' ORDER BY x.id) FROM audit_log x WHERE x.tenant_id = $1)`,
			a.tenantID, idA).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()

	_, err := c.DeleteConnection(b.ctx, &casv1.DeleteConnectionRequest{ConnectionId: idA})
	assertCode(t, "B deletes A's connection", err, codes.NotFound)
	if after := snapshot(); after != before {
		t.Errorf("A's rows or audit log changed:\nbefore %s\nafter  %s", before, after)
	}
	if n := count(t, e, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'CONNECTION_DELETED'`,
		b.tenantID); n != 0 {
		t.Errorf("B's refused delete wrote %d audit rows", n)
	}
}

// C1-T519 — Req: §2.1.1, FR-101. The stream health is written by a real run of the engine: a success of
// balances and a failure of ops.
func TestT519_GetConnection(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	id, k := createExchange(t, c, a, "owner-1")

	// A marker that cannot occur by chance in the response: not hexadecimal, 32 random characters inside.
	// A snapshot run does not move the cursor of balances.
	marker := "cursor-marker-" + randomHex(t, 32) + "-end"
	if _, err := e.owner.Exec(ctx, `UPDATE sync_cursors SET cursor = jsonb_build_object('last_id', $2::text)
		WHERE connection_id = $1 AND stream = 'balances'`, id, marker); err != nil {
		t.Fatal(err)
	}
	e.fake.FailNext("ops", errors.New("fake: ops failed"))
	if err := e.engine().RunPass(ctx); err != nil {
		t.Fatal(err)
	}

	resp, err := c.GetConnection(a.ctx, &casv1.GetConnectionRequest{ConnectionId: id})
	if err != nil {
		t.Fatal(err)
	}
	conn := resp.GetConnection()
	if conn.GetConnectionId() != id || conn.GetSource() != "fake" || conn.GetOwnerRef() != "owner-1" ||
		conn.GetLabel() != "Main account" || conn.GetKind() != casv1.ConnectionKind_CONNECTION_KIND_EXCHANGE ||
		conn.GetStatus() != casv1.ConnectionStatus_CONNECTION_STATUS_ACTIVE ||
		conn.GetKeyFingerprint() != "…"+k.apiKey[len(k.apiKey)-4:] || conn.GetWalletAddress() != "" ||
		!slices.Equal(conn.GetPermissions(), []string{"READ"}) || !conn.GetCreatedAt().AsTime().Equal(testNow) {
		t.Errorf("connection = %v", conn)
	}

	streams := resp.GetStreams()
	if len(streams) != 2 {
		t.Fatalf("streams = %v, want balances and ops", streams)
	}
	balances, ops := streams[0], streams[1]
	if balances.GetStream() != "balances" || balances.GetMode() != casv1.StreamMode_STREAM_MODE_INCREMENTAL ||
		!balances.GetNextRunAt().AsTime().Equal(testNow.Add(15*time.Minute)) ||
		!balances.GetLastSuccessAt().AsTime().Equal(testNow) || balances.GetLastError() != "" ||
		balances.GetConsecutiveFailures() != 0 {
		t.Errorf("balances = %v; want the success of the run", balances)
	}
	if ops.GetStream() != "ops" || ops.GetMode() != casv1.StreamMode_STREAM_MODE_BACKFILL ||
		!ops.GetNextRunAt().AsTime().Equal(testNow.Add(30*time.Second)) || ops.GetLastSuccessAt() != nil ||
		ops.GetLastError() != "fake: ops failed" || ops.GetConsecutiveFailures() != 1 {
		t.Errorf("ops = %v; want the failure of the run", ops)
	}
	if text := resp.String(); strings.Contains(text, marker) || strings.Contains(text, "last_id") {
		t.Error("the response contains the cursor")
	}
}

// C1-T520 — Req: §2.1.1, FR-101
func TestT520_ListConnections(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))

	// 120 connections of two owners; the clock gives every 10 of them the same created_at.
	for i := range 120 {
		e.now = testNow.Add(time.Duration(i/10) * time.Second)
		createWallet(t, c, a, []string{"owner-a", "owner-b"}[i%2])
	}
	order := func(owner string) []string {
		rows, err := e.owner.Query(ctx, `SELECT id::text FROM connections WHERE tenant_id = $1
			AND ($2 = '' OR owner_ref = $2) ORDER BY created_at, id`, a.tenantID, owner)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	all, ownerA := order(""), order("owner-a")
	if len(all) != 120 || len(ownerA) != 60 {
		t.Fatalf("fixture: %d connections, %d of owner-a", len(all), len(ownerA))
	}

	walk := func(owner string, size int32) []string {
		var ids []string
		token := ""
		for range 200 {
			resp, err := c.ListConnections(a.ctx, &casv1.ListConnectionsRequest{OwnerRef: owner, PageSize: size, PageToken: token})
			if err != nil {
				t.Fatalf("page_size %d: %v", size, err)
			}
			for _, conn := range resp.GetConnections() {
				ids = append(ids, conn.GetConnectionId())
			}
			if token = resp.GetNextPageToken(); token == "" {
				return ids
			}
		}
		t.Fatalf("page_size %d: no last page", size)
		return nil
	}
	for _, size := range []int32{1, 7, 10, 11, 50, 0, 120, 500, 501} {
		if got := walk("", size); !slices.Equal(got, all) {
			t.Errorf("page_size %d: %d connections, want all 120 once in the order created_at, connection_id", size, len(got))
		}
	}
	for _, size := range []int32{3, 0} {
		if got := walk("owner-a", size); !slices.Equal(got, ownerA) {
			t.Errorf("owner-a, page_size %d: %d connections, want the 60 of owner-a", size, len(got))
		}
	}

	first, err := c.ListConnections(a.ctx, &casv1.ListConnectionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.GetConnections()) != 100 || first.GetNextPageToken() == "" {
		t.Errorf("page_size 0: %d connections, token %q; want 100 and a token", len(first.GetConnections()), first.GetNextPageToken())
	}
	if got := first.GetConnections()[0]; got.GetWalletAddress() == "" || got.GetKind() != casv1.ConnectionKind_CONNECTION_KIND_EVM_WALLET {
		t.Errorf("listed connection = %v", got)
	}

	validToken := first.GetNextPageToken()
	raw, _ := base64.RawURLEncoding.DecodeString(validToken)
	otherFormat := append([]byte{2}, raw[1:]...)
	// Well-formed tokens of format 1 whose created_at is outside the years 1 to 9999.
	tokenAt := func(micros int64) string {
		b := binary.BigEndian.AppendUint64([]byte{1}, uint64(micros))
		return base64.RawURLEncoding.EncodeToString(append(b, raw[9:]...))
	}
	for name, req := range map[string]*casv1.ListConnectionsRequest{
		"page_size -1":         {PageSize: -1},
		"owner_ref of 129":     {OwnerRef: strings.Repeat("ж", 129)},
		"token not base64":     {PageToken: "not a token!"},
		"token too short":      {PageToken: validToken[:10]},
		"token too long":       {PageToken: validToken + "AA"},
		"token of format 2":    {PageToken: base64.RawURLEncoding.EncodeToString(otherFormat)},
		"token padded base64":  {PageToken: base64.URLEncoding.EncodeToString(raw)},
		"token standard alpha": {PageToken: strings.NewReplacer("-", "+", "_", "/").Replace(validToken) + "+"},
		"token at min int64":   {PageToken: tokenAt(math.MinInt64)},
		"token at max int64":   {PageToken: tokenAt(math.MaxInt64)},
	} {
		_, err := c.ListConnections(a.ctx, req)
		assertCode(t, name, err, codes.InvalidArgument)
	}
	if strings.Contains(e.log.String(), "list connections failed") {
		t.Errorf("a malformed token was logged as a failure:\n%s", e.log.String())
	}

	t.Run("page_size above 500 is cut to 500", func(t *testing.T) {
		if _, err := e.owner.Exec(ctx, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account, created_at)
			SELECT $1, 'owner-c', (SELECT id FROM sources WHERE code = 'anvil'), 'account-' || g, $2
			FROM generate_series(1, 400) g`, a.tenantID, testNow.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		resp, err := c.ListConnections(a.ctx, &casv1.ListConnectionsRequest{PageSize: 501})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetConnections()) != 500 || resp.GetNextPageToken() == "" {
			t.Errorf("page_size 501: %d connections, token %q; want 500 and a token", len(resp.GetConnections()), resp.GetNextPageToken())
		}
	})
}

// C1-T521 — Req: UC-104, FR-101, FR-118, FR-117
func TestT521_Delete(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	id, _ := createExchange(t, c, a, "owner-1")
	addHistory(t, e, a, id)
	if got := rowsOf(t, e, id); slices.Contains(slices.Collect(maps.Values(got)), 0) {
		t.Fatalf("fixture rows = %v, want every table filled", got)
	}

	if _, err := c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: id}); err != nil {
		t.Fatal(err)
	}
	for table, n := range rowsOf(t, e, id) {
		if n != 0 {
			t.Errorf("%s keeps %d rows of the deleted connection", table, n)
		}
	}

	rows, err := e.owner.Query(ctx, `SELECT action, credential_id, details FROM audit_log WHERE object_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	type auditRow struct {
		action     string
		credential uuid.UUID
		details    map[string]any
	}
	audits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (auditRow, error) {
		var a auditRow
		var details []byte
		if err := r.Scan(&a.action, &a.credential, &details); err != nil {
			return a, err
		}
		return a, json.Unmarshal(details, &a.details)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 || audits[0].action != "CONNECTION_CREATED" || audits[1].action != "CONNECTION_DELETED" ||
		audits[1].credential != a.credentialID ||
		fmt.Sprint(audits[1].details) != fmt.Sprint(map[string]any{"source": "fake", "owner_ref": "owner-1"}) {
		t.Errorf("audit rows = %v; want CONNECTION_CREATED and CONNECTION_DELETED with the credential, source and owner_ref", audits)
	}

	// A connection stays readable and deletable when its source is no longer available.
	t.Run("source no longer available", func(t *testing.T) {
		id, _ := createExchange(t, c, a, "owner-2")
		// The row fake is recreated by every setup, so the change does not outlive the test.
		if _, err := e.owner.Exec(ctx, `UPDATE sources SET enabled = false WHERE code = 'fake'`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetConnection(a.ctx, &casv1.GetConnectionRequest{ConnectionId: id}); err != nil {
			t.Errorf("get: %v", err)
		}
		if got := listIDs(t, c, a, &casv1.ListConnectionsRequest{OwnerRef: "owner-2"}); !slices.Equal(got, []string{id}) {
			t.Errorf("list = %v", got)
		}
		if _, err := c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: id}); err != nil {
			t.Errorf("delete: %v", err)
		}
	})
}

// C1-T522 — Req: FR-118. GetConnection and ListConnections. Missing until st8: GetBalances and
// ListLedgerEntries after the deletion.
func TestT522_NothingReadableAfterDeletion(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	id, _ := createExchange(t, c, a, "owner-1")
	kept := createWallet(t, c, a, "owner-1")
	if _, err := c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: id}); err != nil {
		t.Fatal(err)
	}

	_, err := c.GetConnection(a.ctx, &casv1.GetConnectionRequest{ConnectionId: id})
	assertCode(t, "get after delete", err, codes.NotFound)
	if got := listIDs(t, c, a, &casv1.ListConnectionsRequest{}); !slices.Equal(got, []string{kept}) {
		t.Errorf("list after delete = %v, want only %s", got, kept)
	}
}

// C1-T524 — Req: UC-104, §2.1.1
func TestT524_DeleteTwiceOrUnknownID(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	id := createWallet(t, c, a, "owner-1")

	if _, err := c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: strings.ToUpper(id)}); err != nil {
		t.Fatalf("delete with an upper-case ID: %v", err)
	}
	_, err := c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: id})
	assertCode(t, "second delete", err, codes.NotFound)
	_, err = c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: uuid.NewString()})
	assertCode(t, "unknown ID", err, codes.NotFound)
	if n := count(t, e, `SELECT count(*) FROM audit_log WHERE action = 'CONNECTION_DELETED'`); n != 1 {
		t.Errorf("CONNECTION_DELETED rows = %d, want 1", n)
	}

	u := uuid.New()
	for name, bad := range map[string]string{
		"empty":           "",
		"not a UUID":      "connection-1",
		"32 characters":   strings.ReplaceAll(u.String(), "-", ""),
		"braces":          "{" + u.String() + "}",
		"urn":             "urn:uuid:" + u.String(),
		"leading space":   " " + u.String(),
		"one digit short": u.String()[:35],
	} {
		_, err := c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: bad})
		assertCode(t, "delete, "+name, err, codes.InvalidArgument)
		_, err = c.GetConnection(a.ctx, &casv1.GetConnectionRequest{ConnectionId: bad})
		assertCode(t, "get, "+name, err, codes.InvalidArgument)
	}
}

// C1-T525 — Req: UC-104 step 2. The failure is injected through registry.DB, as in T512.
func TestT525_FailedDelete(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	id, _ := createExchange(t, client(e.realServer(t)), a, "owner-1")
	addHistory(t, e, a, id)
	before := rowsOf(t, e, id)

	failing := client(dial(t, api.NewServer(e.deps(failingAuditDB{e.server}))))
	_, err := failing.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: id})
	assertCode(t, "delete with a failing audit insert", err, codes.Internal)
	if msg := status.Convert(err).Message(); msg != "internal error" {
		t.Errorf("message %q, want the fixed one", msg)
	}
	if after := rowsOf(t, e, id); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("rows after the failed delete = %v, want %v", after, before)
	}
	if n := count(t, e, `SELECT count(*) FROM audit_log WHERE action = 'CONNECTION_DELETED'`); n != 0 {
		t.Errorf("CONNECTION_DELETED rows = %d, want none", n)
	}
}

// C1-T526 — Req: FR-104
func TestT526_ConnectAgainAfterDeletion(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	k := newKey(t)

	first, err := c.CreateConnection(a.ctx, exchangeRequest("owner-1", k))
	if err != nil {
		t.Fatal(err)
	}
	oldID := first.GetConnection().GetConnectionId()
	addHistory(t, e, a, oldID)
	if _, err := c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: oldID}); err != nil {
		t.Fatal(err)
	}

	again, err := c.CreateConnection(a.ctx, exchangeRequest("owner-1", k))
	if err != nil {
		t.Fatalf("create the same account again: %v", err)
	}
	newID := again.GetConnection().GetConnectionId()
	if newID == oldID {
		t.Error("the new connection has the old ID")
	}
	rows := rowsOf(t, e, newID)
	if rows["connections"] != 1 || rows["sync_cursors"] != 2 || rows["balance_snapshots"] != 0 || rows["ledger_entries"] != 0 {
		t.Errorf("new connection rows = %v, want fresh cursors and no history", rows)
	}
	var account string
	if err := e.owner.QueryRow(ctx, `SELECT external_account FROM connections WHERE id = $1`, newID).Scan(&account); err != nil {
		t.Fatal(err)
	}
	if account != fake.AccountOf(k.apiKey) {
		t.Errorf("account = %s", account)
	}
}

// C1-T523 — Req: UC-104 step 3
func TestT523_DeleteDuringASync(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	id, _ := createExchange(t, c, a, "owner-1")

	entered, release := make(chan struct{}), make(chan struct{})
	e.fake.SetBefore(func(ctx context.Context, call fake.Call) error {
		if call.Kind == "page" {
			close(entered)
			<-release
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- e.engine().RunPass(ctx) }()
	<-entered // the run is inside a page; its snapshot is stored already
	if _, err := c.DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: id}); err != nil {
		t.Fatalf("delete during the sync: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	for table, n := range rowsOf(t, e, id) {
		if n != 0 {
			t.Errorf("%s holds %d rows of the deleted connection after the run", table, n)
		}
	}
	if n := count(t, e, `SELECT count(*) FROM audit_log WHERE object_id = $1`, id); n != 2 {
		t.Errorf("audit rows of the connection = %d, want CONNECTION_CREATED and CONNECTION_DELETED only", n)
	}
	if strings.Contains(e.log.String(), "cannot store") {
		t.Errorf("the run logged a failure after the deletion:\n%s", e.log.String())
	}
}
