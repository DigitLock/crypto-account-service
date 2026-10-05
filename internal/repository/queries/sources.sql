-- name: ListSources :many
-- Every source, enabled or not. Availability also depends on the registered connectors.
SELECT id, code, kind, enabled, config
FROM sources
ORDER BY code;

-- name: GetSourceByCode :one
SELECT id, code, kind, enabled, config
FROM sources
WHERE code = $1;

-- name: AddFakeSource :execrows
-- Development seed of the fake connector: 1 row when added, 0 when it exists.
INSERT INTO sources (code, kind, enabled, config)
VALUES ('fake', 'EXCHANGE', true, '{}')
ON CONFLICT (code) DO NOTHING;
