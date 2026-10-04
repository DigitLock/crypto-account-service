REVOKE SELECT ON schema_migrations FROM cas_server;
REVOKE SELECT ON tenants, api_credentials, sources, asset_aliases FROM cas_server;
REVOKE INSERT ON audit_log FROM cas_server;
REVOKE SELECT, INSERT ON balance_snapshots, snapshot_balances, ledger_entries FROM cas_server;
REVOKE SELECT, INSERT, UPDATE ON sync_cursors FROM cas_server;
REVOKE SELECT, INSERT, UPDATE, DELETE ON connections FROM cas_server;
