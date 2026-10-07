REVOKE SELECT (block_number) ON operator_txs FROM cas_server;
REVOKE SELECT, INSERT, UPDATE ON balance_checkpoints FROM cas_server;
REVOKE SELECT, INSERT ON reconciliation_runs FROM cas_server;

DROP TABLE balance_checkpoints;
DROP TABLE reconciliation_runs;
