// Package reconcile runs SRS — Card Spend UC-4 for one EVM source: it compares the authorizations and returns of
// card-auth with the Debited and Refunded events stored by the treasury connection and stores one run per tenant
// (S3 D-6, S3 D-25). It reads database tables only and makes no RPC call. It needs the rights of cas_server: reads,
// and INSERT on reconciliation_runs (SRS — Core §3.2). Used by casctl reconcile and by the worker of server.
package reconcile

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Mismatch types (SRS — Core §2.1.1 Reconciliation run; S3 D-21).
const (
	MissingDebit     = "MISSING_DEBIT"
	AmountMismatch   = "AMOUNT_MISMATCH"
	UnknownDebit     = "UNKNOWN_DEBIT"
	UnexpectedDebit  = "UNEXPECTED_DEBIT"
	MissingRefund    = "MISSING_REFUND"
	UnknownRefund    = "UNKNOWN_REFUND"
	TreasuryMismatch = "TREASURY_MISMATCH"
)

// Errors of a source that cannot be reconciled: nothing is stored. An unknown source is registry.ErrSourceNotFound,
// a source of another kind registry.ErrSourceNotEVM.
var (
	// ErrNoTreasury is EC-123: the source has no treasury_connection (SRS — EVM Connector §2.1.1).
	ErrNoTreasury = errors.New("the source has no treasury_connection: set it with casctl source set-treasury")
	// ErrTreasuryNotIndexed: the logs cursor of the treasury connection has no last_time, so no block of it is stored.
	ErrTreasuryNotIndexed = errors.New("the treasury connection has no stored logs yet: its logs cursor has no last_time")
)

// Totals are the counts and sums of a run (SRS — Core §2.1.1; S3 D-40). Amounts in token base units.
type Totals struct {
	AuthorizationsChecked int64  `json:"authorizations_checked"`
	DebitsCount           int64  `json:"debits_count"`
	DebitsAmount          string `json:"debits_amount"`
	ReturnsChecked        int64  `json:"returns_checked"`
	RefundsCount          int64  `json:"refunds_count"`
	RefundsAmount         string `json:"refunds_amount"`
}

// Mismatch is one mismatch of a run (SRS — Core §2.1.1). A field the type has no value for is empty and not stored.
type Mismatch struct {
	Type              string `json:"type"`
	AuthID            string `json:"auth_id,omitempty"`
	ReturnID          string `json:"return_id,omitempty"`
	ChainAuthID       string `json:"chain_auth_id,omitempty"`
	ChainRefundID     string `json:"chain_refund_id,omitempty"`
	TxHash            string `json:"tx_hash,omitempty"`
	ExpectedAmount    string `json:"expected_amount,omitempty"`
	ActualAmount      string `json:"actual_amount,omitempty"`
	Asset             string `json:"asset,omitempty"`
	CheckpointBalance string `json:"checkpoint_balance,omitempty"`
	LedgerTotal       string `json:"ledger_total,omitempty"`
}

// Run is a stored run of one tenant on one source.
type Run struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Tenant     string
	Platform   bool
	PeriodFrom time.Time
	PeriodTo   time.Time
	ToBlock    int64
	Totals     Totals
	Mismatches []Mismatch
	CreatedAt  time.Time
}

// DB is what reconciliation needs from the database. *pgxpool.Pool satisfies it.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Source reconciles the EVM source code and stores its runs: one per partner tenant with an authorization on the
// chain of the source, by name, then the run of the platform tenant. Every input is read and every run inserted in
// one REPEATABLE READ transaction, so the runs agree with one state of the cursor and the ledger. On an error nothing
// is stored.
func Source(ctx context.Context, db DB, code string) ([]Run, error) {
	var runs []Run
	err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		q := repository.New(tx)
		in, err := readInputs(ctx, q, code)
		if err != nil {
			return err
		}
		if runs, err = buildRuns(ctx, q, in); err != nil {
			return err
		}
		for i := range runs {
			if err := store(ctx, q, in.sourceID, &runs[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return runs, nil
}

// inputs are the inputs of a source (UC-4 Events, Checked period).
type inputs struct {
	sourceID  int16
	chainID   int64
	treasury  repository.GetReconcileTreasuryRow
	periodTo  time.Time
	toBlock   int64
	debits    []event
	refunds   []event
	byAuthID  map[string][]event
	byRefund  map[string][]event
	debitIDs  [][]byte
	refundIDs [][]byte
}

// event is a stored Debited or Refunded event of the treasury connection.
type event struct {
	id         string // authId of a Debited, refundId of a Refunded: 0x + 64 hex
	amount     *big.Int
	txHash     string
	occurredAt time.Time
}

func readInputs(ctx context.Context, q *repository.Queries, code string) (inputs, error) {
	src, err := q.GetSourceByCode(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return inputs{}, registry.ErrSourceNotFound
	}
	if err != nil {
		return inputs{}, err
	}
	if src.Kind != connector.KindEVM {
		return inputs{}, registry.ErrSourceNotEVM
	}
	chainID, ok := connector.ChainID(connector.Source{Config: src.Config})
	if !ok || chainID > 1<<63-1 {
		return inputs{}, errors.New("chain_id of the source is not a positive integer")
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(src.Config, &config); err != nil {
		return inputs{}, errors.New("sources.config of the source is not a JSON object")
	}
	var treasuryRef string
	if raw, ok := config[registry.KeyTreasuryConnection]; !ok || json.Unmarshal(raw, &treasuryRef) != nil || treasuryRef == "" {
		return inputs{}, ErrNoTreasury
	}
	treasuryID, err := uuid.Parse(treasuryRef)
	if err != nil {
		return inputs{}, errors.New("treasury_connection of the source is not a connection ID")
	}
	treasury, err := q.GetReconcileTreasury(ctx, treasuryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return inputs{}, errors.New("the treasury connection of the source does not exist")
	}
	if err != nil {
		return inputs{}, err
	}
	if treasury.SourceID != src.ID {
		return inputs{}, errors.New("the treasury connection of the source belongs to another source")
	}

	// period_to and to_block from the logs cursor (S3 D-7; SRS — EVM Connector §2.4 Cursor formats).
	var cur struct {
		NextBlock *uint64 `json:"next_block"`
		LastTime  string  `json:"last_time"`
	}
	if len(treasury.Cursor) > 0 && json.Unmarshal(treasury.Cursor, &cur) != nil {
		return inputs{}, errors.New("the logs cursor of the treasury connection is malformed")
	}
	if cur.LastTime == "" || cur.NextBlock == nil || *cur.NextBlock == 0 {
		return inputs{}, ErrTreasuryNotIndexed
	}
	periodTo, err := time.Parse(time.RFC3339, cur.LastTime)
	if err != nil || *cur.NextBlock > 1<<63-1 {
		return inputs{}, errors.New("the logs cursor of the treasury connection is malformed")
	}

	in := inputs{
		sourceID: src.ID, chainID: int64(chainID), treasury: treasury, periodTo: periodTo.UTC(),
		toBlock: int64(*cur.NextBlock - 1), byAuthID: map[string][]event{}, byRefund: map[string][]event{},
	}
	rows, err := q.ListReconcileEvents(ctx, treasury.ID)
	if err != nil {
		return inputs{}, err
	}
	for _, r := range rows {
		id := r.AuthID
		if r.Type == "CARD_REFUND" {
			id = r.RefundID
		}
		e, raw, err := parseEvent(id, r)
		if err != nil {
			return inputs{}, err
		}
		if r.Type == "CARD_REFUND" {
			in.refunds = append(in.refunds, e)
			in.byRefund[e.id] = append(in.byRefund[e.id], e)
			in.refundIDs = append(in.refundIDs, raw)
		} else {
			in.debits = append(in.debits, e)
			in.byAuthID[e.id] = append(in.byAuthID[e.id], e)
			in.debitIDs = append(in.debitIDs, raw)
		}
	}
	return in, nil
}

// parseEvent checks the fields of a stored log: a 32-byte ID and an amount in base units.
func parseEvent(id string, r repository.ListReconcileEventsRow) (event, []byte, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(id, "0x"))
	if !strings.HasPrefix(id, "0x") || err != nil || len(raw) != 32 {
		return event{}, nil, fmt.Errorf("an event of transaction %s has no 32-byte ID in raw.args", r.TxHash)
	}
	amount, ok := baseUnits(r.Amount)
	if !ok {
		return event{}, nil, fmt.Errorf("an event of transaction %s has no amount in base units in raw.args", r.TxHash)
	}
	return event{id: hexID(raw), amount: amount, txHash: strings.ToLower(r.TxHash), occurredAt: r.OccurredAt}, raw, nil
}

// baseUnits parses a non-negative integer string of digits only.
func baseUnits(s string) (*big.Int, bool) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return nil, false
	}
	return new(big.Int).SetString(s, 10)
}

func hexID(b []byte) string { return "0x" + hex.EncodeToString(b) }

func buildRuns(ctx context.Context, q *repository.Queries, in inputs) ([]Run, error) {
	tenants, err := q.ListReconcileTenants(ctx, repository.ListReconcileTenantsParams{
		PlatformTenantID: in.treasury.TenantID, ChainID: in.chainID,
	})
	if err != nil {
		return nil, err
	}
	runs := make([]Run, 0, len(tenants)+1)
	for _, t := range tenants {
		r, err := partnerRun(ctx, q, in, t.ID)
		if err != nil {
			return nil, err
		}
		r.Tenant = t.Name
		runs = append(runs, r)
	}
	r, err := platformRun(ctx, q, in)
	if err != nil {
		return nil, err
	}
	return append(runs, r), nil
}

// checkedStatus reports whether rule 1 (APPROVED, DEBIT_CONFIRMED) or rule 2a (DECLINED, DEBIT_LOST) checks an
// authorization in this status. Only those count in the totals and the period of a partner run (S3 D-40 refined).
func checkedStatus(status string) bool {
	switch status {
	case "APPROVED", "DEBIT_CONFIRMED", "DECLINED", "DEBIT_LOST":
		return true
	}
	return false
}

// partnerRun applies rules 1, 2a and 3 to the eligible items of one tenant (UC-4; S3 D-25, S3 D-26, S3 D-40):
// authorizations_checked, debits and period_from cover the eligible authorizations of rules 1 and 2a only.
func partnerRun(ctx context.Context, q *repository.Queries, in inputs, tenantID uuid.UUID) (Run, error) {
	auths, err := q.ListReconcileAuthorizations(ctx, repository.ListReconcileAuthorizationsParams{
		TenantID: tenantID, ChainID: in.chainID, PeriodTo: in.periodTo,
	})
	if err != nil {
		return Run{}, err
	}
	returns, err := q.ListReconcileReturns(ctx, repository.ListReconcileReturnsParams{
		TenantID: tenantID, ChainID: in.chainID, ToBlock: in.toBlock,
	})
	if err != nil {
		return Run{}, err
	}
	run := Run{TenantID: tenantID, PeriodFrom: in.periodTo, PeriodTo: in.periodTo, ToBlock: in.toBlock}
	debits, refunds := new(big.Int), new(big.Int)
	for _, a := range auths {
		if !checkedStatus(a.Status) {
			continue // no rule for it; a late debit is checked through its LATE_DEBIT return (rule 3)
		}
		if run.Totals.AuthorizationsChecked == 0 {
			run.PeriodFrom = a.ReceivedAt.UTC() // oldest first
		}
		run.Totals.AuthorizationsChecked++
		chainAuthID := hexID(a.ChainAuthID)
		events := in.byAuthID[chainAuthID]
		for _, e := range events {
			run.Totals.DebitsCount++
			debits.Add(debits, e.amount)
		}
		switch a.Status {
		case "APPROVED", "DEBIT_CONFIRMED": // rule 1
			if len(events) == 0 {
				run.Mismatches = append(run.Mismatches, Mismatch{Type: MissingDebit, AuthID: a.AuthID,
					ChainAuthID: chainAuthID, ExpectedAmount: a.TokenAmount, TxHash: a.TxHash})
			}
			for _, e := range events {
				if !sameAmount(a.TokenAmount, e.amount) {
					run.Mismatches = append(run.Mismatches, Mismatch{Type: AmountMismatch, AuthID: a.AuthID,
						ChainAuthID: chainAuthID, TxHash: e.txHash, ExpectedAmount: a.TokenAmount, ActualAmount: e.amount.String()})
				}
			}
		case "DECLINED", "DEBIT_LOST": // rule 2a
			for _, e := range events {
				run.Mismatches = append(run.Mismatches, Mismatch{Type: UnexpectedDebit, AuthID: a.AuthID,
					ChainAuthID: chainAuthID, TxHash: e.txHash, ActualAmount: e.amount.String()})
			}
		}
	}
	for _, r := range returns { // rule 3
		run.Totals.ReturnsChecked++
		chainRefundID := hexID(r.ChainRefundID)
		events := in.byRefund[chainRefundID]
		if len(events) == 0 {
			run.Mismatches = append(run.Mismatches, Mismatch{Type: MissingRefund, ReturnID: r.ReturnID,
				ChainRefundID: chainRefundID, ExpectedAmount: r.TokenAmount, TxHash: r.TxHash})
		}
		for _, e := range events {
			run.Totals.RefundsCount++
			refunds.Add(refunds, e.amount)
			if !sameAmount(r.TokenAmount, e.amount) {
				run.Mismatches = append(run.Mismatches, Mismatch{Type: AmountMismatch, ReturnID: r.ReturnID,
					ChainRefundID: chainRefundID, TxHash: e.txHash, ExpectedAmount: r.TokenAmount, ActualAmount: e.amount.String()})
			}
		}
	}
	run.Totals.DebitsAmount, run.Totals.RefundsAmount = debits.String(), refunds.String()
	sortMismatches(run.Mismatches)
	return run, nil
}

// platformRun applies rules 2, 4 and 5 to every event of the treasury (UC-4; S3 D-6, S3 D-26, S3 D-40). It never
// holds an auth_id or a return_id.
func platformRun(ctx context.Context, q *repository.Queries, in inputs) (Run, error) {
	knownAuth, err := known(ctx, q.ListKnownChainAuthIDs, in.debitIDs)
	if err != nil {
		return Run{}, err
	}
	knownRefund, err := known(ctx, q.ListKnownChainRefundIDs, in.refundIDs)
	if err != nil {
		return Run{}, err
	}
	checkpoints, err := q.ListReconcileCheckpoints(ctx, in.treasury.ID)
	if err != nil {
		return Run{}, err
	}
	run := Run{TenantID: in.treasury.TenantID, Tenant: in.treasury.TenantName, Platform: true,
		PeriodFrom: in.periodTo, PeriodTo: in.periodTo, ToBlock: in.toBlock}
	debits, refunds := new(big.Int), new(big.Int)
	oldest := func(e event) {
		if e.occurredAt.Before(run.PeriodFrom) {
			run.PeriodFrom = e.occurredAt.UTC()
		}
	}
	for _, e := range in.debits { // rule 2
		run.Totals.DebitsCount++
		debits.Add(debits, e.amount)
		oldest(e)
		if !knownAuth[e.id] {
			run.Mismatches = append(run.Mismatches, Mismatch{Type: UnknownDebit, ChainAuthID: e.id, TxHash: e.txHash,
				ActualAmount: e.amount.String()})
		}
	}
	for _, e := range in.refunds { // rule 4
		run.Totals.RefundsCount++
		refunds.Add(refunds, e.amount)
		oldest(e)
		if !knownRefund[e.id] {
			run.Mismatches = append(run.Mismatches, Mismatch{Type: UnknownRefund, ChainRefundID: e.id, TxHash: e.txHash,
				ActualAmount: e.amount.String()})
		}
	}
	for _, c := range checkpoints { // rule 5
		run.Mismatches = append(run.Mismatches, Mismatch{Type: TreasuryMismatch, Asset: c.Asset,
			CheckpointBalance: c.Balance, LedgerTotal: c.LedgerTotal})
	}
	run.Totals.DebitsAmount, run.Totals.RefundsAmount = debits.String(), refunds.String()
	sortMismatches(run.Mismatches)
	return run, nil
}

// known returns which of the IDs the query finds, as 0x hex.
func known(ctx context.Context, list func(context.Context, [][]byte) ([][]byte, error), ids [][]byte) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	found, err := list(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range found {
		out[hexID(id)] = true
	}
	return out, nil
}

// sameAmount compares an amount of CAS, an integer string, with the amount of an event. An amount of CAS that is
// not an integer never matches.
func sameAmount(expected string, actual *big.Int) bool {
	n, ok := baseUnits(expected)
	return ok && n.Cmp(actual) == 0
}

// sortMismatches orders by type, then chain ID, then tx_hash; TREASURY_MISMATCH, without either, by asset.
func sortMismatches(ms []Mismatch) {
	slices.SortStableFunc(ms, func(a, b Mismatch) int {
		return cmp.Or(
			strings.Compare(a.Type, b.Type),
			strings.Compare(a.ChainAuthID+a.ChainRefundID, b.ChainAuthID+b.ChainRefundID),
			strings.Compare(a.TxHash, b.TxHash),
			strings.Compare(a.Asset, b.Asset),
		)
	})
}

// store inserts the run; the stored mismatches are an empty array when there are none.
func store(ctx context.Context, q *repository.Queries, sourceID int16, run *Run) error {
	if run.Mismatches == nil {
		run.Mismatches = []Mismatch{}
	}
	totals, err := json.Marshal(run.Totals)
	if err != nil {
		return err
	}
	mismatches, err := json.Marshal(run.Mismatches)
	if err != nil {
		return err
	}
	row, err := q.InsertReconciliationRun(ctx, repository.InsertReconciliationRunParams{
		TenantID: run.TenantID, SourceID: sourceID, PeriodFrom: run.PeriodFrom, PeriodTo: run.PeriodTo,
		ToBlock: run.ToBlock, Totals: totals, Mismatches: mismatches,
	})
	if err != nil {
		return fmt.Errorf("store the run: %w", err)
	}
	run.ID, run.CreatedAt = row.ID, row.CreatedAt
	return nil
}

// Counts returns the number of mismatches per type, in the order of the types.
func Counts(ms []Mismatch) []TypeCount {
	var out []TypeCount
	for _, m := range ms {
		if i := slices.IndexFunc(out, func(c TypeCount) bool { return c.Type == m.Type }); i >= 0 {
			out[i].Count++
			continue
		}
		out = append(out, TypeCount{Type: m.Type, Count: 1})
	}
	return out
}

// TypeCount is the number of mismatches of one type.
type TypeCount struct {
	Type  string
	Count int
}
