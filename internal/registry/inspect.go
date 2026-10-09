package registry

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Inspection is what casctl connection inspect shows of a connection ID (X1 D-45): the state of the connection when
// it exists, and what is stored for its ID either way. It holds no account identity, uid, asset, amount or
// fingerprint.
type Inspection struct {
	Found         bool
	Status        string
	Permissions   []string
	HasCiphertext bool
	// IPRestricted is ip_restricted of the CONNECTION_CREATED audit row: "true", "false", or empty when not recorded.
	IPRestricted  string
	Snapshots     int64
	BalanceRows   int64
	Cursors       int64
	LedgerEntries int64
	// Audit holds the audit rows of the connection per action, ordered by action.
	Audit []AuditCount
	// AuditHasUID reports whether the details of any audit row of the connection hold a member uid.
	AuditHasUID bool
}

// AuditCount is the number of audit rows of one action.
type AuditCount struct {
	Action string
	Rows   int64
}

// InspectConnection reads the Inspection of a connection ID on the owner role.
func (r *Registry) InspectConnection(ctx context.Context, id uuid.UUID) (Inspection, error) {
	q := repository.New(r.pool)
	var in Inspection
	row, err := q.InspectConnection(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Inspection{}, err
	default:
		in.Found, in.Status, in.Permissions, in.HasCiphertext = true, row.Status, row.Permissions, row.HasCiphertext
	}
	ip, err := q.InspectIPRestricted(ctx, id.String())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Inspection{}, err
	}
	in.IPRestricted = ip
	res, err := q.InspectResiduals(ctx, id)
	if err != nil {
		return Inspection{}, err
	}
	in.Snapshots, in.BalanceRows, in.Cursors, in.LedgerEntries = res.Snapshots, res.BalanceRows, res.Cursors, res.LedgerEntries
	audit, err := q.InspectAudit(ctx, id.String())
	if err != nil {
		return Inspection{}, err
	}
	for _, a := range audit {
		in.Audit = append(in.Audit, AuditCount{Action: a.Action, Rows: a.RowCount})
		in.AuditHasUID = in.AuditHasUID || a.HasUid
	}
	return in, nil
}
