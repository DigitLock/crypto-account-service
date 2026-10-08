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

-- name: LockSourceByCode :one
-- casctl source set: the source row, locked until the end of the transaction.
SELECT id, code, kind, enabled, config
FROM sources
WHERE code = $1
FOR UPDATE;

-- name: SetSourceConfigValue :exec
-- One key of sources.config; the other keys are kept.
UPDATE sources
SET config = config || jsonb_build_object(sqlc.arg(key)::text, sqlc.arg(value)::jsonb)
WHERE id = sqlc.arg(id);

-- name: ListSourceAliasesOfAsset :many
-- The aliases of a source that map to one canonical asset: the tracked token of an EVM source.
SELECT native_asset
FROM asset_aliases
WHERE source_id = $1 AND asset = $2
ORDER BY native_asset;

-- name: DeleteSourceAliasesOfAsset :exec
DELETE FROM asset_aliases
WHERE source_id = $1 AND asset = $2;

-- name: InsertSourceAlias :exec
INSERT INTO asset_aliases (source_id, native_asset, asset, decimals)
VALUES ($1, $2, $3, $4);

-- name: ListAliases :many
-- Every alias row with the code of its source: the Source of a connector carries the rows of its source (S3 D-31).
SELECT s.code AS source_code, a.native_asset, a.asset, a.decimals
FROM asset_aliases a
JOIN sources s ON s.id = a.source_id
ORDER BY s.code, a.native_asset;

-- name: ListAliasesOfSource :many
-- The alias rows of one source.
SELECT a.native_asset, a.asset, a.decimals
FROM asset_aliases a
JOIN sources s ON s.id = a.source_id
WHERE s.code = $1
ORDER BY a.native_asset;

-- name: GetConnectionOfSource :one
-- casctl source set-treasury: a connection, its wallet address and the code and kind of its source.
SELECT c.id, c.external_account, s.code AS source_code, s.kind AS source_kind
FROM connections c
JOIN sources s ON s.id = c.source_id
WHERE c.id = $1;
