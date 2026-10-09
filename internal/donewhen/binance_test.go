package donewhen

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// binanceFixtures is testdata/fixtures/binance/ of the repository.
var binanceFixtures = filepath.Join(repoRoot(), "testdata", "fixtures", "binance")

// beyondReading are the permissions of UC-201 step 3 as of 2026-10-09 and an unknown one (X1 P-5).
var beyondReading = []string{
	"enableWithdrawals", "enableInternalTransfer", "permitsUniversalTransfer", "enableSpotAndMarginTrading",
	"enableMargin", "enableFutures", "enableVanillaOptions", "enablePortfolioMarginTrading", "enableFixApiTrade",
	"enableNewThing",
}

func binanceCalls(t *testing.T, name string) []httpfixture.Call {
	t.Helper()
	f, err := httpfixture.Read(filepath.Join(binanceFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return f.Calls
}

// restrictionsWith returns the apiRestrictions call of key_read_only.json with one permission set to true.
func restrictionsWith(t *testing.T, field string) httpfixture.Call {
	t.Helper()
	c := binanceCalls(t, "key_read_only.json")[1]
	var body map[string]any
	if err := json.Unmarshal(c.Body, &body); err != nil {
		t.Fatal(err)
	}
	body[field] = true
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	c.Body = b
	return c
}

// X1-T503 — Req: done-when 1; FR-201; X1 P-5. server and casctl as built binaries on the test database, the source
// binance pointing at a fake Binance server: CreateConnection through the gRPC API of the running server with each
// of the nine permissions beyond reading and an unknown one is FAILED_PRECONDITION / KEY_NOT_READ_ONLY naming it,
// and nothing is stored; a read-only key is ACTIVE with ["READ"], and the engine of the running server writes its
// first balance snapshot. server has no fake clock: the engine runs on SYNC_TICK 200ms.
func TestT503_DoneWhenKeyNotReadOnlyWithBinaries(t *testing.T) {
	start := time.Now()
	owner := testdb.Open(t)
	testdb.Clean(t)

	// The answers in the order of the calls: the time once (the offset is kept per process), one apiRestrictions per
	// rejected key, then the read-only key and the first snapshot of the engine.
	readOnly := binanceCalls(t, "key_read_only.json")
	calls := []httpfixture.Call{readOnly[0]}
	for _, p := range beyondReading {
		calls = append(calls, restrictionsWith(t, p))
	}
	calls = append(calls, readOnly[1:]...)
	calls = append(calls, binanceCalls(t, "snapshot_full.json")[1:]...)
	fake := httpfixture.Serve(t, httpfixture.File{Description: "done-when 1", Calls: calls}, nil)

	var saved []byte
	if err := owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'binance'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(ctx, `UPDATE sources SET config = $1 WHERE code = 'binance'`, saved) })
	if _, err := owner.Exec(ctx, `UPDATE sources SET config = config || jsonb_build_object('base_url', $1::text)
		WHERE code = 'binance'`, fake.URL()); err != nil {
		t.Fatal(err)
	}

	casctlEnv := []string{"CASCTL_DATABASE_URL=" + testdb.URL(t)}
	runCasctl(t, casctlEnv, "tenant", "create", "x1-done-when")
	token := strings.TrimSpace(runCasctl(t, casctlEnv, "token", "issue", "x1-done-when"))

	grpcPort, health := freePort(t), freePort(t)
	server := startProcess(t, "server", bin.server, map[string]string{
		"DATABASE_URL":     testdb.ServerURL(t),
		"CAS_MASTER_KEY":   masterKey(t),
		"GRPC_PORT":        strconv.Itoa(grpcPort),
		"HEALTH_HTTP_PORT": strconv.Itoa(health),
		"SYNC_TICK":        "200ms",
		"SHUTDOWN_TIMEOUT": "5s",
	}, health)
	connections := casv1.NewConnectionServiceClient(grpcClient(t, grpcPort))
	request := func() (*casv1.CreateConnectionRequest, string, string) {
		key, secret := hex.EncodeToString(randomBytes(t, 16)), hex.EncodeToString(randomBytes(t, 32))
		return &casv1.CreateConnectionRequest{OwnerRef: "owner-x1", Source: "binance", Label: "Binance",
			Credential: &casv1.CreateConnectionRequest_ExchangeKey{ExchangeKey: &casv1.ExchangeKey{ApiKey: key, ApiSecret: secret}}}, key, secret
	}

	var secrets []string
	for _, p := range beyondReading {
		req, key, secret := request()
		secrets = append(secrets, key, secret)
		_, err := connections.CreateConnection(bearer(token), req)
		st := status.Convert(err)
		reason := ""
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				reason = info.GetReason()
			}
		}
		if st.Code() != codes.FailedPrecondition || reason != "KEY_NOT_READ_ONLY" || st.Message() != "the key is not read-only: "+p {
			t.Errorf("%s: %v %s %q, want FAILED_PRECONDITION KEY_NOT_READ_ONLY naming it", p, st.Code(), reason, st.Message())
		}
	}
	var stored int
	if err := owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM connections) + (SELECT count(*) FROM sync_cursors)
		+ (SELECT count(*) FROM audit_log WHERE action = 'CONNECTION_CREATED')`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("%d rows of connections, cursors and CONNECTION_CREATED after the rejections, want none", stored)
	}

	req, key, secret := request()
	secrets = append(secrets, key, secret)
	resp, err := connections.CreateConnection(bearer(token), req)
	if err != nil {
		t.Fatalf("read-only key: %v", err)
	}
	c := resp.GetConnection()
	if c.GetStatus() != casv1.ConnectionStatus_CONNECTION_STATUS_ACTIVE || !slices.Equal(c.GetPermissions(), []string{"READ"}) {
		t.Fatalf("read-only key: %v, want ACTIVE with [READ]", c)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var snapshots int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, c.GetConnectionId()).
			Scan(&snapshots); err != nil {
			t.Fatal(err)
		}
		if snapshots == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no balance snapshot within 30 s:\n%s", server.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	balances, err := casv1.NewAccountDataServiceClient(grpcClient(t, grpcPort)).GetBalances(bearer(token),
		&casv1.GetBalancesRequest{Selector: &casv1.GetBalancesRequest_ConnectionId{ConnectionId: c.GetConnectionId()}})
	if err != nil || len(balances.GetBalances()) != 8 {
		t.Errorf("GetBalances: %v, %d balances; want the 8 of snapshot_full.json", err, len(balances.GetBalances()))
	}
	fake.AssertAllServed()
	for _, s := range secrets {
		if strings.Contains(server.logs.String(), s) {
			t.Error("the log of server contains a key or a secret of the test")
		}
	}
	t.Logf("done-when 1 with the binaries: %v", time.Since(start).Round(time.Millisecond))
}
