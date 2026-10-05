REVOKE SELECT (
    id, tenant_id, owner_ref, source_id, external_account, label, status, kek_version, key_fingerprint,
    permissions, permissions_checked_at, last_manual_sync_at, created_at
) ON connections FROM cas_card_auth;
REVOKE SELECT ON cards, tenants, api_credentials, sources FROM cas_card_auth;
REVOKE INSERT ON audit_log FROM cas_card_auth;
REVOKE USAGE ON SEQUENCE authorization_events_id_seq FROM cas_card_auth;
REVOKE SELECT, INSERT ON authorization_events FROM cas_card_auth;
REVOKE SELECT, INSERT, UPDATE ON authorizations, returns, operator_txs, operator_accounts FROM cas_card_auth;

REVOKE SELECT (authorization_id, return_row_id, purpose, status, tx_hash, created_at) ON operator_txs FROM cas_server;
REVOKE SELECT ON authorizations, authorization_events, returns FROM cas_server;
REVOKE SELECT, INSERT, UPDATE ON cards FROM cas_server;
