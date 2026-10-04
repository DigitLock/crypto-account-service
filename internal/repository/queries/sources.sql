-- name: ListSources :many
-- Every source, enabled or not. Availability also depends on the registered connectors.
SELECT id, code, kind, enabled, config
FROM sources
ORDER BY code;

-- name: GetSourceByCode :one
SELECT id, code, kind, enabled, config
FROM sources
WHERE code = $1;
