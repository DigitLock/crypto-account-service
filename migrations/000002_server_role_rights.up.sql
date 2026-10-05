-- Rights of the role cas_server (SRS - Core §3.2). The operator creates the role once per environment.

GRANT SELECT, INSERT, UPDATE, DELETE ON connections TO cas_server;
GRANT SELECT, INSERT, UPDATE ON sync_cursors TO cas_server;
GRANT SELECT, INSERT ON balance_snapshots, snapshot_balances, ledger_entries TO cas_server;
GRANT INSERT ON audit_log TO cas_server;
GRANT SELECT ON tenants, api_credentials, sources, asset_aliases TO cas_server;
-- /readyz compares the schema version with the one the binary expects.
GRANT SELECT ON schema_migrations TO cas_server;
