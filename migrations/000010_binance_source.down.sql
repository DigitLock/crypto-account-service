-- Removes what 000010_binance_source.up.sql added: the alias rows of binance and the source.

DELETE FROM asset_aliases WHERE source_id = (SELECT id FROM sources WHERE code = 'binance');
DELETE FROM sources WHERE code = 'binance';
