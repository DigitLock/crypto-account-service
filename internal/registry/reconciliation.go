package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// ErrRunNotFound: no run of the tenant on the source, or the run_id is of another tenant or another source.
var ErrRunNotFound = errors.New("reconciliation run not found")

// ReconciliationRun is a stored run of SRS — Core §2.1.1 Reconciliation run.
type ReconciliationRun struct {
	ID         uuid.UUID
	Source     string
	PeriodFrom time.Time
	PeriodTo   time.Time
	ToBlock    int64
	Totals     RunTotals
	Mismatches []RunMismatch
	CreatedAt  time.Time
}

// RunTotals are the stored totals of a run; amounts in token base units.
type RunTotals struct {
	AuthorizationsChecked int64  `json:"authorizations_checked"`
	DebitsCount           int64  `json:"debits_count"`
	DebitsAmount          string `json:"debits_amount"`
	ReturnsChecked        int64  `json:"returns_checked"`
	RefundsCount          int64  `json:"refunds_count"`
	RefundsAmount         string `json:"refunds_amount"`
}

// RunMismatch is a stored mismatch; a field absent in the stored JSON is empty.
type RunMismatch struct {
	Type              string `json:"type"`
	AuthID            string `json:"auth_id"`
	ReturnID          string `json:"return_id"`
	ChainAuthID       string `json:"chain_auth_id"`
	ChainRefundID     string `json:"chain_refund_id"`
	TxHash            string `json:"tx_hash"`
	ExpectedAmount    string `json:"expected_amount"`
	ActualAmount      string `json:"actual_amount"`
	Asset             string `json:"asset"`
	CheckpointBalance string `json:"checkpoint_balance"`
	LedgerTotal       string `json:"ledger_total"`
}

// ReconciliationRun returns a run of the tenant on the source (SRS — Core §2.1.1 GetReconciliationReport): the run
// runID, or the newest when runID is nil. An unknown source is ErrSourceNotFound; no such run of the tenant on the
// source is ErrRunNotFound. It reads only: the runs are written by internal/reconcile.
func (c *Cards) ReconciliationRun(ctx context.Context, tenantID uuid.UUID, source string, runID *uuid.UUID) (ReconciliationRun, error) {
	q := repository.New(c.db)
	if _, err := q.GetSourceByCode(ctx, source); errors.Is(err, pgx.ErrNoRows) {
		return ReconciliationRun{}, ErrSourceNotFound
	} else if err != nil {
		return ReconciliationRun{}, fmt.Errorf("read the source: %w", err)
	}
	row, err := q.GetReconciliationRun(ctx, repository.GetReconciliationRunParams{TenantID: tenantID, SourceCode: source, RunID: runID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReconciliationRun{}, ErrRunNotFound
	}
	if err != nil {
		return ReconciliationRun{}, fmt.Errorf("read the run: %w", err)
	}
	run := ReconciliationRun{ID: row.ID, Source: row.SourceCode, PeriodFrom: row.PeriodFrom, PeriodTo: row.PeriodTo,
		ToBlock: row.ToBlock, CreatedAt: row.CreatedAt, Mismatches: []RunMismatch{}}
	if err := json.Unmarshal(row.Totals, &run.Totals); err != nil {
		return ReconciliationRun{}, fmt.Errorf("the totals of run %s are malformed: %w", row.ID, err)
	}
	if err := json.Unmarshal(row.Mismatches, &run.Mismatches); err != nil {
		return ReconciliationRun{}, fmt.Errorf("the mismatches of run %s are malformed: %w", row.ID, err)
	}
	return run, nil
}
