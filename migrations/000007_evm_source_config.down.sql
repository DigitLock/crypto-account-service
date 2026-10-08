DELETE FROM asset_aliases
WHERE native_asset = '0x6c0434c821694513FFfd5364D63f27F05d73f3aB'
    AND source_id = (SELECT id FROM sources WHERE code = 'base-sepolia');

UPDATE sources SET config = config - ARRAY['finality_mode', 'finality_tag', 'controller_address', 'backfill_floor']
WHERE code = 'base-sepolia';

UPDATE sources SET config = config - ARRAY['finality_mode', 'finality_confirmations']
WHERE code = 'anvil';
