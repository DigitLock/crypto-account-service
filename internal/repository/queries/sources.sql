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

-- name: AddFakeAlias :execrows
-- Alias of an asset of the fake source: 1 row when added, 0 when it exists.
INSERT INTO asset_aliases (source_id, native_asset, asset)
SELECT id, sqlc.arg(native_asset), sqlc.arg(asset) FROM sources WHERE code = 'fake'
ON CONFLICT (source_id, native_asset) DO NOTHING;
