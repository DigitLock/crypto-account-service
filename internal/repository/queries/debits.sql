-- Debit path of card-auth (SRS - Card Spend UC-1 steps 10 - 13), role cas_card_auth.

-- name: SubmitAuthorization :execrows
-- Step 10: RECEIVED -> DEBIT_SUBMITTED with the on-chain expiry of the debit.
UPDATE authorizations
SET status = 'DEBIT_SUBMITTED', valid_until = sqlc.arg(valid_until)
WHERE id = sqlc.arg(id) AND status = 'RECEIVED';

-- name: FinishSubmittedAuthorization :execrows
-- Steps 12 - 13: DEBIT_SUBMITTED -> APPROVED, DECLINED or TIMED_OUT. debited_amount is the token amount only
-- for an approval.
UPDATE authorizations
SET status         = sqlc.arg(status),
    decline_reason = sqlc.narg(decline_reason),
    debited_amount = CASE WHEN sqlc.arg(debited)::bool THEN token_amount ELSE debited_amount END,
    decided_at     = sqlc.arg(decided_at)
WHERE id = sqlc.arg(id) AND status = 'DEBIT_SUBMITTED';

-- name: SetOperatorTxOutcome :exec
-- Step 12: INCLUDED or REVERTED. block_number and block_hash come only from a sealed receipt: null for a
-- preconfirmed one (UC-3 row 10).
UPDATE operator_txs
SET status = sqlc.arg(status), block_number = sqlc.narg(block_number), block_hash = sqlc.narg(block_hash)
WHERE id = sqlc.arg(id);
