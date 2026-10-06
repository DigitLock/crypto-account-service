package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/DigitLock/crypto-account-service/internal/decision"
)

// errDebitNotBuilt: steps 10 to 13 of UC-1 are not built yet (S2 st5b).
var errDebitNotBuilt = errors.New("debit path not built")

// notBuiltDebit is the Debit step of card-auth until the operator queue exists: it fails closed, so an
// authorization that passed every check is declined as INTERNAL_ERROR and no transaction is sent.
type notBuiltDebit struct{ logger *slog.Logger }

func (d notBuiltDebit) Debit(ctx context.Context, a decision.Checked) (decision.Decision, error) {
	d.logger.ErrorContext(ctx, "debit path not built", "tenant_id", a.TenantID.String(), "auth_id", a.AuthID)
	return decision.Decision{}, errDebitNotBuilt
}
