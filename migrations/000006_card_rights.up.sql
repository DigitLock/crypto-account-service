-- Rights on the card tables (SRS - Core §3.2). The operator creates the roles once per environment.
-- No role gets DELETE on a card table.

-- cas_server: the card registry; reads of the authorizations, never their writes.
GRANT SELECT, INSERT, UPDATE ON cards TO cas_server;
GRANT SELECT ON authorizations, authorization_events, returns TO cas_server;
-- The transaction hashes of the reads (D-11): these columns of operator_txs only; nothing of operator_accounts.
GRANT SELECT (authorization_id, return_row_id, purpose, status, tx_hash, created_at) ON operator_txs TO cas_server;

-- cas_card_auth: decisions, returns and operator transactions.
GRANT SELECT, INSERT, UPDATE ON authorizations, returns, operator_txs, operator_accounts TO cas_card_auth;
-- authorization_events is append-only.
GRANT SELECT, INSERT ON authorization_events TO cas_card_auth;
GRANT USAGE ON SEQUENCE authorization_events_id_seq TO cas_card_auth;
GRANT INSERT ON audit_log TO cas_card_auth;
GRANT SELECT ON cards, tenants, api_credentials, sources TO cas_card_auth;
-- Every column but credentials_enc: card-auth cannot read exchange secrets.
GRANT SELECT (
    id, tenant_id, owner_ref, source_id, external_account, label, status, kek_version, key_fingerprint,
    permissions, permissions_checked_at, last_manual_sync_at, created_at
) ON connections TO cas_card_auth;
