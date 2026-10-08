// Phase 6 of docs/test-plan-s3.md, rows T610 and T612: GetReconciliationReport of the server of cmd/server in
// process, on the pool of cas_server. The treasury, its cursor, its entries and the authorizations are rows written
// by the owner role; the runs are stored by internal/reconcile as the worker and casctl store them. The rules on
// Anvil are covered in internal/reconcile.
package api_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/reconcile"
)

// recSetup names a treasury connection of cas-platform on anvil with a logs cursor at block 120 (last_time 10:00),
// restoring the config of anvil at the end of the test, and returns the connection ID.
func recSetup(t *testing.T, e *env) uuid.UUID {
	t.Helper()
	var saved []byte
	if err := e.owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := e.owner.Exec(ctx, `UPDATE sources SET config = $1 WHERE code = 'anvil'`, saved); err != nil {
			t.Error(err)
		}
	})
	var id uuid.UUID
	if err := e.owner.QueryRow(ctx, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account, label)
		SELECT t.id, 'treasury', s.id, $1, 'Treasury' FROM tenants t, sources s WHERE t.name = 'cas-platform' AND s.code = 'anvil'
		RETURNING id`, "0x"+randomHex(t, 40)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE sources SET config = config || jsonb_build_object('treasury_connection', $1::text) WHERE code = 'anvil'`, id.String())
	e.exec(t, `INSERT INTO sync_cursors (connection_id, stream, mode, cursor, next_run_at) VALUES ($1, 'logs', 'INCREMENTAL',
		'{"next_block": 121, "last_hash": "0x01", "last_time": "2026-10-07T10:00:00Z"}', now())`, id)
	return id
}

// recAuthorization writes an authorization of a tenant on anvil, received at 09:00 + n s and valid 4 s later, and
// returns its chain_auth_id: a random 32-byte ID.
func recAuthorization(t *testing.T, e *env, tenant, authID, status string, amount int64, n int) string {
	t.Helper()
	chainAuthID := randomHex(t, 64)
	at := time.Date(2026, 10, 7, 9, 0, n, 0, time.UTC)
	e.exec(t, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, token_amount, chain_id, status, received_at,
		valid_until) SELECT id, $2, decode($3, 'hex'), $4, 31337, $5, $6, $7 FROM tenants WHERE name = $1`,
		tenant, authID, chainAuthID, amount, status, at, at.Add(4*time.Second))
	return "0x" + chainAuthID
}

// recDebit writes a Debited entry of the treasury connection, as the connector stores it, and returns its tx_hash.
func recDebit(t *testing.T, e *env, treasury uuid.UUID, chainAuthID string, amount int64) string {
	t.Helper()
	tx := "0x" + randomHex(t, 64)
	e.exec(t, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id, type, direction,
		asset, native_asset, amount, occurred_at, raw) SELECT tenant_id, id, 'logs', $2 || ':1', 'SINGLE', 'logs:' || $2 || ':1',
		'CARD_DEBIT', 'IN', 'USDC', '0x00000000000000000000000000000000000A6100', $4::numeric / 1000000, '2026-10-07T09:30:00Z',
		jsonb_build_object('transactionHash', $2::text, 'event', 'Debited',
			'args', jsonb_build_object('authId', $3::text, 'amount', $4::text))
		FROM connections WHERE id = $1`, treasury, tx, chainAuthID, amount)
	return tx
}

func report(t *testing.T, client casv1.CardServiceClient, who caller, source, runID string) (*casv1.ReconciliationRun, error) {
	t.Helper()
	resp, err := client.GetReconciliationReport(who.ctx, &casv1.GetReconciliationReportRequest{Source: source, RunId: runID})
	return resp.GetRun(), err
}

// S3-T610 — Req: Card Spend UC-4 runs, Core FR-105; S3 D-6. Tenants A and B with authorizations on anvil, a
// MISSING_DEBIT of B, an unknown debit: A's token gets A's run only; B's run_id with A's token is NOT_FOUND; B sees
// its MISSING_DEBIT; the cas-platform run holds no auth_id or return_id; tenant D without authorizations on the
// chain has no run.
func TestT610_TenantScope(t *testing.T) {
	e := setup(t)
	a, b, d := e.caller(t, "tenant-a"), e.caller(t, "tenant-b"), e.caller(t, "tenant-d")
	platform := e.caller(t, "cas-platform")
	treasury := recSetup(t, e)
	recDebit(t, e, treasury, recAuthorization(t, e, "tenant-a", "a1", "DEBIT_CONFIRMED", 20_000_000, 1), 20_000_000)
	recAuthorization(t, e, "tenant-b", "b1", "APPROVED", 8_000_000, 2)
	unknownTx := recDebit(t, e, treasury, "0x"+randomHex(t, 64), 3_000_000)
	runs, err := reconcile.Source(ctx, e.server, "anvil")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("%d runs, want tenant-a, tenant-b, cas-platform", len(runs))
	}
	runOf := map[string]reconcile.Run{}
	for _, r := range runs {
		runOf[r.Tenant] = r
	}
	client := cardClient(e.realServer(t))

	got, err := report(t, client, a, "anvil", "")
	if err != nil || got.GetRunId() != runOf["tenant-a"].ID.String() || len(got.GetMismatches()) != 0 {
		t.Errorf("A's newest run: %v, %v; want %s without mismatch", got, err, runOf["tenant-a"].ID)
	}
	_, err = report(t, client, a, "anvil", runOf["tenant-b"].ID.String())
	assertCode(t, "B's run_id with A's token", err, codes.NotFound)
	_, err = report(t, client, a, "anvil", runOf["cas-platform"].ID.String())
	assertCode(t, "the cas-platform run_id with A's token", err, codes.NotFound)

	got, err = report(t, client, b, "anvil", runOf["tenant-b"].ID.String())
	if err != nil || len(got.GetMismatches()) != 1 || got.GetMismatches()[0].GetAuthId() != "b1" ||
		got.GetMismatches()[0].GetType() != casv1.ReconciliationMismatchType_RECONCILIATION_MISMATCH_TYPE_MISSING_DEBIT {
		t.Errorf("B's run: %v, %v; want its MISSING_DEBIT of b1", got, err)
	}

	got, err = report(t, client, platform, "anvil", "")
	if err != nil || len(got.GetMismatches()) != 1 || got.GetMismatches()[0].GetTxHash() != unknownTx {
		t.Fatalf("cas-platform run: %v, %v; want its UNKNOWN_DEBIT", got, err)
	}
	for _, m := range got.GetMismatches() {
		if m.GetAuthId() != "" || m.GetReturnId() != "" {
			t.Errorf("the cas-platform run holds an ID of a tenant: %v", m)
		}
	}

	_, err = report(t, client, d, "anvil", "")
	assertCode(t, "tenant D without authorizations on the chain", err, codes.NotFound)
	var n int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, d.tenantID).Scan(&n); err != nil || n != 0 {
		t.Errorf("tenant D: %d runs, %v; want none", n, err)
	}
}

// S3-T612 — Req: Core §2.1.1; S3 D-10, S3 D-24. The answers of GetReconciliationReport: empty source, unknown source,
// malformed run_id: INVALID_ARGUMENT, NOT_FOUND, INVALID_ARGUMENT; empty run_id: the newest run of the caller for
// the source; a run_id of another source and no run yet: NOT_FOUND. The shape of the totals and of each mismatch
// type, from a stored row.
func TestT612_GetReconciliationReport(t *testing.T) {
	e := setup(t)
	a, c := e.caller(t, "tenant-a"), e.caller(t, "tenant-c")
	e.caller(t, "cas-platform")
	treasury := recSetup(t, e)
	recDebit(t, e, treasury, recAuthorization(t, e, "tenant-a", "a1", "DEBIT_CONFIRMED", 20_000_000, 1), 20_000_000)
	first, err := reconcile.Source(ctx, e.server, "anvil")
	if err != nil {
		t.Fatal(err)
	}
	recAuthorization(t, e, "tenant-a", "a2", "APPROVED", 5_000_000, 2)
	second, err := reconcile.Source(ctx, e.server, "anvil")
	if err != nil {
		t.Fatal(err)
	}
	client := cardClient(e.realServer(t))

	for name, tc := range map[string]struct {
		source, runID string
		want          codes.Code
	}{
		"empty source":                {"", "", codes.InvalidArgument},
		"unknown source":              {"polygon", "", codes.NotFound},
		"malformed run_id":            {"anvil", "not-a-uuid", codes.InvalidArgument},
		"run_id without dashes":       {"anvil", "0b0b0b0b00004000800000000000dead", codes.InvalidArgument},
		"run_id of another source":    {"base-sepolia", first[0].ID.String(), codes.NotFound},
		"no run yet on the source":    {"base-sepolia", "", codes.NotFound},
		"unknown run_id":              {"anvil", "0b0b0b0b-0000-4000-8000-00000000dead", codes.NotFound},
		"source of kind EXCHANGE":     {"fake", "", codes.NotFound},
		"empty source with a run_id":  {"", first[0].ID.String(), codes.InvalidArgument},
		"malformed run_id, no source": {"", "x", codes.InvalidArgument},
	} {
		_, err := report(t, client, a, tc.source, tc.runID)
		assertCode(t, name, err, tc.want)
	}
	_, err = report(t, client, c, "anvil", "")
	assertCode(t, "a tenant without a run", err, codes.NotFound)

	newest, err := report(t, client, a, "anvil", "")
	if err != nil || newest.GetRunId() != second[0].ID.String() {
		t.Errorf("empty run_id: %v, %v; want the newest run %s", newest, err, second[0].ID)
	}
	older, err := report(t, client, a, "anvil", first[0].ID.String())
	want := &casv1.ReconciliationRun{
		RunId: first[0].ID.String(), Source: "anvil",
		PeriodFrom: timestamppb.New(time.Date(2026, 10, 7, 9, 0, 1, 0, time.UTC)),
		PeriodTo:   timestamppb.New(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)),
		ToBlock:    "120", CreatedAt: timestamppb.New(first[0].CreatedAt),
		Totals: &casv1.ReconciliationTotals{AuthorizationsChecked: 1, DebitsCount: 1, DebitsAmount: "20000000",
			ReturnsChecked: 0, RefundsCount: 0, RefundsAmount: "0"},
		Mismatches: nil,
	}
	if err != nil || !proto.Equal(older, want) {
		t.Errorf("the run by run_id:\n%v, %v\nwant\n%v", older, err, want)
	}

	t.Run("shape of every mismatch type", func(t *testing.T) {
		id, hash, refund := "0x"+randomHex(t, 64), "0x"+randomHex(t, 64), "0x"+randomHex(t, 64)
		mismatches := `[
			{"type": "MISSING_DEBIT", "auth_id": "a1", "chain_auth_id": "` + id + `", "expected_amount": "8000000"},
			{"type": "AMOUNT_MISMATCH", "auth_id": "a1", "chain_auth_id": "` + id + `", "tx_hash": "` + hash + `", "expected_amount": "25000000", "actual_amount": "20000000"},
			{"type": "AMOUNT_MISMATCH", "return_id": "r1", "chain_refund_id": "` + refund + `", "tx_hash": "` + hash + `", "expected_amount": "6000000", "actual_amount": "5000000"},
			{"type": "UNKNOWN_DEBIT", "chain_auth_id": "` + id + `", "tx_hash": "` + hash + `", "actual_amount": "3000000"},
			{"type": "UNEXPECTED_DEBIT", "auth_id": "a2", "chain_auth_id": "` + id + `", "tx_hash": "` + hash + `", "actual_amount": "6000000"},
			{"type": "MISSING_REFUND", "return_id": "r1", "chain_refund_id": "` + refund + `", "tx_hash": "` + hash + `", "expected_amount": "5000000"},
			{"type": "UNKNOWN_REFUND", "chain_refund_id": "` + refund + `", "tx_hash": "` + hash + `", "actual_amount": "3000000"},
			{"type": "TREASURY_MISMATCH", "asset": "USDC", "checkpoint_balance": "70", "ledger_total": "-1.5"}]`
		var runID uuid.UUID
		if err := e.owner.QueryRow(ctx, `INSERT INTO reconciliation_runs (tenant_id, source_id, period_from, period_to, to_block,
			totals, mismatches) SELECT $1, id, '2026-10-07T09:00:00Z', '2026-10-07T10:00:00Z', 7, $2, $3 FROM sources
			WHERE code = 'base-sepolia' RETURNING id`, c.tenantID,
			`{"authorizations_checked": 3, "debits_count": 2, "debits_amount": "30000000", "returns_checked": 1,
			"refunds_count": 1, "refunds_amount": "5000000"}`, mismatches).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		got, err := report(t, client, c, "base-sepolia", "")
		if err != nil {
			t.Fatal(err)
		}
		type_ := func(name string) casv1.ReconciliationMismatchType {
			return casv1.ReconciliationMismatchType(casv1.ReconciliationMismatchType_value["RECONCILIATION_MISMATCH_TYPE_"+name])
		}
		wantMismatches := []*casv1.ReconciliationMismatch{
			{Type: type_("MISSING_DEBIT"), AuthId: "a1", ChainAuthId: id, ExpectedAmount: "8000000"},
			{Type: type_("AMOUNT_MISMATCH"), AuthId: "a1", ChainAuthId: id, TxHash: hash, ExpectedAmount: "25000000", ActualAmount: "20000000"},
			{Type: type_("AMOUNT_MISMATCH"), ReturnId: "r1", ChainRefundId: refund, TxHash: hash, ExpectedAmount: "6000000", ActualAmount: "5000000"},
			{Type: type_("UNKNOWN_DEBIT"), ChainAuthId: id, TxHash: hash, ActualAmount: "3000000"},
			{Type: type_("UNEXPECTED_DEBIT"), AuthId: "a2", ChainAuthId: id, TxHash: hash, ActualAmount: "6000000"},
			{Type: type_("MISSING_REFUND"), ReturnId: "r1", ChainRefundId: refund, TxHash: hash, ExpectedAmount: "5000000"},
			{Type: type_("UNKNOWN_REFUND"), ChainRefundId: refund, TxHash: hash, ActualAmount: "3000000"},
			{Type: type_("TREASURY_MISMATCH"), Asset: "USDC", CheckpointBalance: "70", LedgerTotal: "-1.5"},
		}
		for _, m := range wantMismatches {
			if m.GetType() == casv1.ReconciliationMismatchType_RECONCILIATION_MISMATCH_TYPE_UNSPECIFIED {
				t.Fatalf("a type of the test is not in the enum: %v", m)
			}
		}
		wantRun := &casv1.ReconciliationRun{
			RunId: runID.String(), Source: "base-sepolia",
			PeriodFrom: timestamppb.New(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)),
			PeriodTo:   timestamppb.New(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)),
			ToBlock:    "7", CreatedAt: got.GetCreatedAt(),
			Totals: &casv1.ReconciliationTotals{AuthorizationsChecked: 3, DebitsCount: 2, DebitsAmount: "30000000",
				ReturnsChecked: 1, RefundsCount: 1, RefundsAmount: "5000000"},
			Mismatches: wantMismatches,
		}
		if !proto.Equal(got, wantRun) || got.GetCreatedAt().AsTime().IsZero() {
			t.Errorf("run:\n%v\nwant\n%v", got, wantRun)
		}
	})
}
